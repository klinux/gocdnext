package deploysvc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var (
	convergedState   = deploy.DeployState{Sync: deploy.SyncSynced, Health: deploy.HealthHealthy, ObservedRev: "rev1"}
	degradedState    = deploy.DeployState{Sync: deploy.SyncOutOfSync, Health: deploy.HealthDegraded}
	progressingState = deploy.DeployState{Sync: deploy.SyncOutOfSync, Health: deploy.HealthProgressing}
)

func mkWatch(app string) store.DeployWatch {
	now := time.Now()
	req := now.Add(-1 * time.Minute)
	return store.DeployWatch{
		DeploymentRevisionID: uuid.New(),
		ProjectID:            uuid.New(),
		ClaimID:              uuid.New(),
		SyncMode:             "trigger",
		Cluster:              "prod",
		Application:          app,
		Namespace:            "argocd",
		ExpectedRevision:     "rev1",
		SyncRequestedAt:      &req,
		DeadlineAt:           now.Add(1 * time.Hour),
	}
}

type obsResult struct {
	state  deploy.DeployState
	err    error
	panics bool
}

type fakeObserver struct {
	byApp map[string]obsResult
	log   *[]string
}

func (f *fakeObserver) Observe(_ context.Context, t deploy.DeploymentTarget) (deploy.DeployState, error) {
	*f.log = append(*f.log, "observe:"+t.Application)
	r := f.byApp[t.Application]
	if r.panics {
		panic("observe boom")
	}
	return r.state, r.err
}

// fakeWatchStore records the driver's calls and controls each fenced op's result.
// renewSeq drives successive Renew returns per revision (default true); setOK/finalOK
// default true.
type fakeWatchStore struct {
	toClaim       []store.DeployWatch
	renewSeq      map[uuid.UUID][]bool
	renewIdx      map[uuid.UUID]int
	setOK         map[uuid.UUID]bool
	finalOK       map[uuid.UUID]bool
	finalized     map[uuid.UUID]string
	finalizeRunID uuid.UUID // returned as DeployWatchFinalizeResult.RunID
	lastRollout   store.RolloutObservationInput
	log           *[]string
}

func newFakeStore(log *[]string, watches ...store.DeployWatch) *fakeWatchStore {
	return &fakeWatchStore{
		toClaim: watches, renewSeq: map[uuid.UUID][]bool{}, renewIdx: map[uuid.UUID]int{},
		setOK: map[uuid.UUID]bool{}, finalOK: map[uuid.UUID]bool{}, finalized: map[uuid.UUID]string{},
		log: log,
	}
}

func (f *fakeWatchStore) ClaimDeployWatches(_ context.Context, _ string, _, _ int) ([]store.DeployWatch, error) {
	return f.toClaim, nil
}

func (f *fakeWatchStore) RenewDeployWatch(_ context.Context, revID, _ uuid.UUID) (bool, error) {
	*f.log = append(*f.log, "renew")
	seq := f.renewSeq[revID]
	i := f.renewIdx[revID]
	f.renewIdx[revID]++
	if i < len(seq) {
		return seq[i], nil
	}
	return true, nil
}

func (f *fakeWatchStore) SetDeployWatchDegradedSince(_ context.Context, revID, _ uuid.UUID) (bool, error) {
	*f.log = append(*f.log, "setdeg")
	return okDefaultTrue(f.setOK, revID), nil
}

func (f *fakeWatchStore) ClearDeployWatchDegraded(_ context.Context, _, _ uuid.UUID) (bool, error) {
	*f.log = append(*f.log, "cleardeg")
	return true, nil
}

func (f *fakeWatchStore) StampRolloutObservation(_ context.Context, _, _ uuid.UUID, in store.RolloutObservationInput) (bool, error) {
	*f.log = append(*f.log, "rollout_stamp")
	f.lastRollout = in
	return true, nil
}

func (f *fakeWatchStore) FinalizeDeployWatch(_ context.Context, revID, _ uuid.UUID, status, _ string) (store.DeployWatchFinalizeResult, error) {
	*f.log = append(*f.log, "finalize:"+status)
	ok := okDefaultTrue(f.finalOK, revID)
	if ok {
		f.finalized[revID] = status
	}
	return store.DeployWatchFinalizeResult{Finalized: ok, RunID: f.finalizeRunID}, nil
}

func (f *fakeWatchStore) NotifyRunQueued(_ context.Context, runID uuid.UUID) error {
	*f.log = append(*f.log, "notify:"+runID.String())
	return nil
}

func okDefaultTrue(m map[uuid.UUID]bool, id uuid.UUID) bool {
	if v, ok := m[id]; ok {
		return v
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func runTick(obs Observer, st WatchStore) {
	NewWatcher(obs, st, "worker", testLogger()).Tick(context.Background())
}

// Happy path: renew BEFORE the observe I/O, then renew again BEFORE the terminal
// write, then finalize success.
func TestWatcher_Success_OrderAndFinalize(t *testing.T) {
	var log []string
	w := mkWatch("app")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: convergedState}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if want := []string{"renew", "observe:app", "renew", "finalize:success"}; !equal(log, want) {
		t.Fatalf("call order = %v, want %v", log, want)
	}
	if st.finalized[w.DeploymentRevisionID] != store.DeployStatusSuccess {
		t.Fatalf("finalized = %q, want success", st.finalized[w.DeploymentRevisionID])
	}
}

// A rollout-aware watch persists the observed snapshot each tick (before Decide,
// which is unchanged in observe-only PR1). A non-converged app just Continues.
func TestWatcher_RolloutAware_StampsSnapshot(t *testing.T) {
	var log []string
	w := mkWatch("app")
	w.RolloutAware = true
	st := newFakeStore(&log, w)
	state := deploy.DeployState{
		Sync: deploy.SyncSynced, Health: deploy.HealthProgressing,
		ObservedRev: "rev1", RolloutObserved: true,
		Rollout: deploy.RolloutState{
			Phase: deploy.RolloutPaused, PauseReason: "CanaryPauseStep",
			CurrentStepIndex: 1, CurrentStepKnown: true, StepCount: 4, PausedIndefinitely: true,
		},
	}
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: state}}, log: &log}
	runTick(obs, st)

	if !contains(log, "rollout_stamp") {
		t.Fatalf("rollout snapshot not stamped: %v", log)
	}
	if !st.lastRollout.Observed || st.lastRollout.Phase != "Paused" || !st.lastRollout.StepKnown ||
		st.lastRollout.CurrentStep != 1 || st.lastRollout.StepCount != 4 {
		t.Errorf("stamped input = %+v, want the observed paused snapshot", st.lastRollout)
	}
}

// On an Application observe error, a rollout-aware watch clears its snapshot (writes
// rollout_error) so the UI doesn't show ghost progress — with a FIXED reason, never
// the raw error (which can carry the internal API-server URL).
func TestWatcher_RolloutAware_ObserveError_ClearsSnapshot(t *testing.T) {
	var log []string
	w := mkWatch("app")
	w.RolloutAware = true
	st := newFakeStore(&log, w)
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {err: errors.New(`Get "https://internal-api:6443/...": timeout`)}}, log: &log}
	runTick(obs, st)

	if !contains(log, "rollout_stamp") {
		t.Fatalf("expected a rollout stamp on the observe-error path: %v", log)
	}
	if st.lastRollout.Observed || st.lastRollout.Error == "" {
		t.Errorf("want Observed=false + a reason, got %+v", st.lastRollout)
	}
	if strings.Contains(st.lastRollout.Error, "internal-api") || strings.Contains(st.lastRollout.Error, "6443") {
		t.Errorf("stamped reason leaked the raw error: %q", st.lastRollout.Error)
	}
}

// A non-rollout-aware watch never stamps — no wasted write.
func TestWatcher_NotRolloutAware_NoStamp(t *testing.T) {
	var log []string
	w := mkWatch("app") // RolloutAware false
	st := newFakeStore(&log, w)
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: convergedState}}, log: &log}
	runTick(obs, st)
	if contains(log, "rollout_stamp") {
		t.Fatalf("stamped for a non-rollout-aware watch: %v", log)
	}
}

// After a successful finalize that completed a server-managed job, the watcher
// NOTIFYs the run so the scheduler advances the next stage promptly.
func TestWatcher_Finalize_NotifiesRun(t *testing.T) {
	var log []string
	w := mkWatch("app")
	runID := uuid.New()
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: convergedState}}, log: &log}
	st := newFakeStore(&log, w)
	st.finalizeRunID = runID
	runTick(obs, st)

	if !contains(log, "finalize:success") {
		t.Fatalf("did not finalize: %v", log)
	}
	if !contains(log, "notify:"+runID.String()) {
		t.Fatalf("did not NOTIFY the run after finalize: %v", log)
	}
}

// A lost lease on the pre-observe renew drops the watch: no observe, no finalize.
func TestWatcher_LeaseLostBeforeObserve(t *testing.T) {
	var log []string
	w := mkWatch("app")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: convergedState}}, log: &log}
	st := newFakeStore(&log, w)
	st.renewSeq[w.DeploymentRevisionID] = []bool{false}
	runTick(obs, st)

	if !equal(log, []string{"renew"}) {
		t.Fatalf("log = %v, want just [renew] (dropped on lost lease)", log)
	}
	if len(st.finalized) != 0 {
		t.Fatal("finalized despite a lost lease")
	}
}

// An Observe error never finalizes — it logs and retries next tick until the deadline.
func TestWatcher_ObserveError_DoesNotFinalize(t *testing.T) {
	var log []string
	w := mkWatch("app")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {err: errors.New("dial timeout")}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !equal(log, []string{"renew", "observe:app"}) {
		t.Fatalf("log = %v, want [renew observe:app] (no finalize/degraded)", log)
	}
	if len(st.finalized) != 0 {
		t.Fatal("finalized on an observe error")
	}
}

// A past-deadline target we can't observe must not poll forever: the deadline still
// fails it (deadline exceeded) via the terminal path.
func TestWatcher_ObserveError_PastDeadline_Finalizes(t *testing.T) {
	var log []string
	w := mkWatch("app")
	w.DeadlineAt = time.Now().Add(-1 * time.Second) // budget already blown
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {err: errors.New("dial timeout")}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if st.finalized[w.DeploymentRevisionID] != store.DeployStatusFailed {
		t.Fatalf("finalized = %q, want failed (deadline exceeded); log=%v", st.finalized[w.DeploymentRevisionID], log)
	}
}

// A SetDegraded verdict opens the debounce window; nothing terminal happens.
func TestWatcher_SetDegraded(t *testing.T) {
	var log []string
	w := mkWatch("app") // DegradedSince nil → first Degraded tick
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: degradedState}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !equal(log, []string{"renew", "observe:app", "setdeg"}) {
		t.Fatalf("log = %v, want set-degraded, no finalize", log)
	}
}

// FinalizeDeployWatch fenced-false (lease lost, or the job path already cleaned up):
// clean drop, nothing recorded, no crash.
func TestWatcher_FinalizeFencedFalse_CleanDrop(t *testing.T) {
	var log []string
	w := mkWatch("app")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: convergedState}}, log: &log}
	st := newFakeStore(&log, w)
	st.finalOK[w.DeploymentRevisionID] = false
	runTick(obs, st)

	if len(st.finalized) != 0 {
		t.Fatal("recorded a finalize the fence rejected")
	}
	if want := []string{"renew", "observe:app", "renew", "finalize:success"}; !equal(log, want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
}

// A lost lease on the pre-finalize renew skips the terminal write entirely.
func TestWatcher_LeaseLostBeforeFinalize(t *testing.T) {
	var log []string
	w := mkWatch("app")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: convergedState}}, log: &log}
	st := newFakeStore(&log, w)
	st.renewSeq[w.DeploymentRevisionID] = []bool{true, false} // ok pre-observe, lost pre-finalize
	runTick(obs, st)

	if contains(log, "finalize:success") {
		t.Fatalf("finalized after losing the lease pre-finalize: %v", log)
	}
	if len(st.finalized) != 0 {
		t.Fatal("finalized after a lost lease")
	}
}

// Continue mutates nothing.
func TestWatcher_Continue_NoMutation(t *testing.T) {
	var log []string
	w := mkWatch("app")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: progressingState}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !equal(log, []string{"renew", "observe:app"}) {
		t.Fatalf("log = %v, want just renew+observe", log)
	}
	if len(st.finalized) != 0 {
		t.Fatal("Continue must not finalize")
	}
}

// Isolation: a panic while processing one watch must not stall the rest of the batch.
func TestWatcher_Isolation_PanicDoesNotStallBatch(t *testing.T) {
	var log []string
	a := mkWatch("a-panics")
	b := mkWatch("b-ok")
	obs := &fakeObserver{byApp: map[string]obsResult{
		"a-panics": {panics: true},
		"b-ok":     {state: convergedState},
	}, log: &log}
	st := newFakeStore(&log, a, b)
	runTick(obs, st)

	if st.finalized[b.DeploymentRevisionID] != store.DeployStatusSuccess {
		t.Fatalf("watch B not finalized after A panicked; log=%v finalized=%v", log, st.finalized)
	}
}
