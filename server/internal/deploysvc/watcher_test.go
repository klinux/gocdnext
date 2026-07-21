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
	byApp      map[string]obsResult
	promoteErr error
	abortErr   error
	gotPromote *deploy.DeploymentTarget
	gotAbort   *deploy.DeploymentTarget
	log        *[]string
}

func (f *fakeObserver) Observe(_ context.Context, t deploy.DeploymentTarget) (deploy.DeployState, error) {
	*f.log = append(*f.log, "observe:"+t.Application)
	r := f.byApp[t.Application]
	if r.panics {
		panic("observe boom")
	}
	return r.state, r.err
}

func (f *fakeObserver) Promote(_ context.Context, t deploy.DeploymentTarget) error {
	*f.log = append(*f.log, "promote:"+t.RolloutName)
	tt := t
	f.gotPromote = &tt
	return f.promoteErr
}

func (f *fakeObserver) Abort(_ context.Context, t deploy.DeploymentTarget) error {
	*f.log = append(*f.log, "abort:"+t.RolloutName)
	tt := t
	f.gotAbort = &tt
	return f.abortErr
}

// fakeWatchStore records the driver's calls and controls each fenced op's result.
// renewSeq drives successive Renew returns per revision (default true); setOK/finalOK
// default true.
type fakeWatchStore struct {
	toClaim         []store.DeployWatch
	renewSeq        map[uuid.UUID][]bool
	renewIdx        map[uuid.UUID]int
	setOK           map[uuid.UUID]bool
	finalOK         map[uuid.UUID]bool
	finalized       map[uuid.UUID]string
	finalizeRunID   uuid.UUID // returned as DeployWatchFinalizeResult.RunID
	lastRollout     store.RolloutObservationInput
	armOK           map[uuid.UUID]bool
	actionedOK      map[uuid.UUID]bool
	clearOK         map[uuid.UUID]bool
	abortActionedOK map[uuid.UUID]bool
	cancelReq       *time.Time // returned by DeployWatchCancelRequestedAt (nil = not canceled)
	cancelReadErr   error      // when set, DeployWatchCancelRequestedAt fails (transient blip)
	lastArm         store.ArmRolloutGateInput
	log             *[]string
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

func (f *fakeWatchStore) ArmRolloutGate(_ context.Context, revID, _ uuid.UUID, in store.ArmRolloutGateInput) (bool, error) {
	*f.log = append(*f.log, "arm_gate:"+in.RolloutName)
	f.lastArm = in
	return okDefaultTrue(f.armOK, revID), nil
}

func (f *fakeWatchStore) MarkGateActioned(_ context.Context, revID, _ uuid.UUID) (bool, error) {
	*f.log = append(*f.log, "mark_actioned")
	return okDefaultTrue(f.actionedOK, revID), nil
}

func (f *fakeWatchStore) ClearRolloutGate(_ context.Context, revID, _ uuid.UUID) (bool, error) {
	*f.log = append(*f.log, "clear_gate")
	return okDefaultTrue(f.clearOK, revID), nil
}

func (f *fakeWatchStore) MarkRolloutAbortActioned(_ context.Context, revID, _ uuid.UUID) (bool, error) {
	*f.log = append(*f.log, "mark_abort_actioned")
	return okDefaultTrue(f.abortActionedOK, revID), nil
}

func (f *fakeWatchStore) DeployWatchCancelRequestedAt(_ context.Context, _ uuid.UUID) (*time.Time, error) {
	if f.cancelReadErr != nil {
		return nil, f.cancelReadErr
	}
	return f.cancelReq, nil
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

func runTick(obs DeployProvider, st WatchStore) {
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

// --- Phase 2 gate effect dispatch ---

func rolloutObs(r deploy.RolloutState) deploy.DeployState {
	return deploy.DeployState{Sync: deploy.SyncSynced, Health: deploy.HealthHealthy, ObservedRev: "rev1", Rollout: r, RolloutObserved: true}
}

func obsIdentity(r deploy.RolloutState) deploy.RolloutState {
	r.ResolvedCluster, r.ResolvedNamespace, r.ResolvedName = "dest", "ns", "ro"
	return r
}

var (
	obsPausedIndef = obsIdentity(deploy.RolloutState{Phase: deploy.RolloutPaused, PauseReason: deploy.PauseReasonCanaryStep, CurrentStepIndex: 1, CurrentStepKnown: true, StepCount: 3, PausedIndefinitely: true, StableHash: "a", PodHash: "b"})
	obsProgressing = obsIdentity(deploy.RolloutState{Phase: deploy.RolloutProgressing, CurrentStepIndex: 2, CurrentStepKnown: true, StepCount: 3})
	obsPromoted    = obsIdentity(deploy.RolloutState{Phase: deploy.RolloutHealthy, CurrentStepIndex: 3, CurrentStepKnown: true, StepCount: 3, FullyPromoted: true, StableHash: "b", PodHash: "b"})
)

func mkRolloutWatch(app string) store.DeployWatch {
	w := mkWatch(app)
	w.RolloutAware = true
	return w
}
func gatedW(w store.DeployWatch) store.DeployWatch { w.Gated = true; return w }
func armedW(w store.DeployWatch) store.DeployWatch {
	at := time.Now().Add(-5 * time.Minute)
	w.GateArmedAt = &at
	w.GatePausedStep, w.GatePausedStepKnown = 1, true
	// A distinct PINNED name so a test can prove Promote/Abort use the pin, not the
	// this-tick observed identity.
	w.GateRolloutCluster, w.GateRolloutNamespace, w.GateRolloutName = "dest", "ns", "ro-pinned"
	return w
}
func decidedW(w store.DeployWatch, d string) store.DeployWatch { w.GateDecision = d; return w }

// indexOf returns the position of s in xs, or -1.
func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// An indefinite pause on a gated deploy arms the gate with the OBSERVED identity + step.
func TestWatcher_ArmGate(t *testing.T) {
	var log []string
	w := gatedW(mkRolloutWatch("app"))
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPausedIndef)}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !contains(log, "arm_gate:ro") {
		t.Fatalf("expected arm_gate with the observed identity; log=%v", log)
	}
	if st.lastArm.PausedStep != 1 || st.lastArm.RolloutCluster != "dest" || st.lastArm.RolloutNamespace != "ns" || st.lastArm.RolloutName != "ro" {
		t.Errorf("arm input = %+v, want step 1 + observed dest/ns/ro", st.lastArm)
	}
}

// An approved gate promotes ONCE via the PINNED identity, renewing right before the
// actuation, then marks it actioned.
func TestWatcher_Promote_RenewsAndUsesPin(t *testing.T) {
	var log []string
	w := decidedW(armedW(gatedW(mkRolloutWatch("app"))), "approved")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPausedIndef)}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !contains(log, "promote:ro-pinned") {
		t.Fatalf("expected promote via the PINNED name; log=%v", log)
	}
	// renew right before the actuation, and mark actioned only after it.
	promote := indexOf(log, "promote:ro-pinned")
	if last := lastRenewBefore(log, promote); last < 0 || last != promote-1 {
		t.Errorf("no renew immediately before promote; log=%v", log)
	}
	if mark := indexOf(log, "mark_actioned"); mark < 0 || mark < promote {
		t.Errorf("mark_actioned missing or before promote; log=%v", log)
	}
	if obs.gotPromote == nil || obs.gotPromote.RolloutName != "ro-pinned" {
		t.Errorf("promote target = %+v, want the pin ro-pinned", obs.gotPromote)
	}
}

// A rejected gate aborts via the pin, then marks actioned.
func TestWatcher_Reject_Aborts(t *testing.T) {
	var log []string
	w := decidedW(armedW(gatedW(mkRolloutWatch("app"))), "rejected")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPausedIndef)}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !contains(log, "abort:ro-pinned") || !contains(log, "mark_actioned") {
		t.Fatalf("expected abort(pin)+mark_actioned; log=%v", log)
	}
}

// If the actuation itself fails, gate_actioned_at is NOT stamped (retry next tick).
func TestWatcher_Promote_ActuationFailure_NoMark(t *testing.T) {
	var log []string
	w := decidedW(armedW(gatedW(mkRolloutWatch("app"))), "approved")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPausedIndef)}}, promoteErr: errors.New("api down"), log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !contains(log, "promote:ro-pinned") {
		t.Fatalf("expected the promote attempt; log=%v", log)
	}
	if contains(log, "mark_actioned") {
		t.Errorf("marked actioned despite a failed actuation; log=%v", log)
	}
}

// An approved+actioned gate whose rollout LEFT the step clears the gate.
func TestWatcher_ClearGate_AfterAdvance(t *testing.T) {
	var log []string
	w := decidedW(armedW(gatedW(mkRolloutWatch("app"))), "approved")
	at := time.Now()
	w.GateActionedAt = &at // already promoted
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsProgressing)}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !contains(log, "clear_gate") {
		t.Fatalf("expected clear_gate after the rollout advanced; log=%v", log)
	}
}

// No early finalize: rollout-aware + observed + App Synced+Healthy but NOT FullyPromoted
// → Continue, no finalize.
func TestWatcher_NoEarlyFinalize(t *testing.T) {
	var log []string
	w := mkRolloutWatch("app") // rollout-aware, not gated
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsProgressing)}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if len(st.finalized) != 0 {
		t.Fatalf("finalized while the canary was mid-progress: %v", st.finalized)
	}
	// FullyPromoted → success.
	var log2 []string
	obs2 := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPromoted)}}, log: &log2}
	st2 := newFakeStore(&log2, mkRolloutWatch("app2"))
	// map the app name
	obs2.byApp["app2"] = obs2.byApp["app"]
	NewWatcher(obs2, st2, "worker", testLogger()).Tick(context.Background())
	if !contains(log2, "finalize:success") {
		t.Fatalf("FullyPromoted rollout did not finalize success; log=%v", log2)
	}
}

// lastRenewBefore returns the index of the last "renew" strictly before pos, or -1.
func lastRenewBefore(log []string, pos int) int {
	last := -1
	for i := 0; i < pos && i < len(log); i++ {
		if log[i] == "renew" {
			last = i
		}
	}
	return last
}

// --- Observe-error path must still run Decide (Phase-2 precedence under uncertainty) ---

// A human REJECT still aborts via the pin even when the Application can't be read
// (abort is safe under uncertainty).
func TestWatcher_ObserveError_RejectStillAborts(t *testing.T) {
	var log []string
	w := decidedW(armedW(gatedW(mkRolloutWatch("app"))), "rejected")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {err: errors.New(`Get "https://internal:6443": timeout`)}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if !contains(log, "abort:ro-pinned") || !contains(log, "mark_actioned") {
		t.Fatalf("reject under an observe error must still abort via the pin; log=%v", log)
	}
}

// An armed & undecided gate SUSPENDS the deadline even under an observe error — a
// past-deadline tick must NOT finalize (it's awaiting the human).
func TestWatcher_ObserveError_ArmedUndecided_SuspendsDeadline(t *testing.T) {
	var log []string
	w := armedW(gatedW(mkRolloutWatch("app"))) // armed, undecided
	w.DeadlineAt = time.Now().Add(-time.Second)
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {err: errors.New("dial timeout")}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if len(st.finalized) != 0 {
		t.Fatalf("armed & undecided past deadline must be suspended (no finalize); finalized=%v log=%v", st.finalized, log)
	}
}

// APPROVE stays promote-unsafe under an observe error — no promote until observation
// recovers (or the resumed deadline fails it).
func TestWatcher_ObserveError_ApproveDoesNotPromote(t *testing.T) {
	var log []string
	w := decidedW(armedW(gatedW(mkRolloutWatch("app"))), "approved")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {err: errors.New("dial timeout")}}, log: &log}
	st := newFakeStore(&log, w)
	runTick(obs, st)

	if contains(log, "promote:ro-pinned") {
		t.Fatalf("approve under an observe error must NOT promote (promote-unsafe); log=%v", log)
	}
	if len(st.finalized) != 0 {
		t.Fatalf("must not finalize while awaiting recovery within the deadline; %v", st.finalized)
	}
}

// --- Phase 2 cancel/supersede abort ---

// A non-gated rollout deploy that's cancel-requested aborts via the OBSERVED identity and
// marks the gate-INDEPENDENT guard (mark_abort_actioned), never the gated mark_actioned.
func TestWatcher_CancelAbort_NonGated_UsesObservedIdentity(t *testing.T) {
	var log []string
	w := mkRolloutWatch("app") // rollout-aware, NOT gated, no pin
	st := newFakeStore(&log, w)
	now := time.Now()
	st.cancelReq = &now
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsProgressing)}}, log: &log}
	runTick(obs, st)

	if !contains(log, "abort:ro") || !contains(log, "mark_abort_actioned") {
		t.Fatalf("cancel-abort should abort(observed ro) + mark_abort_actioned; log=%v", log)
	}
	if contains(log, "mark_actioned") {
		t.Fatalf("cancel-abort must NOT use the gated mark_actioned; log=%v", log)
	}
	if obs.gotAbort == nil || obs.gotAbort.RolloutName != "ro" {
		t.Errorf("abort target = %+v, want the observed identity ro", obs.gotAbort)
	}
	// Renew precedes the abort (single-actuator contract).
	if a := indexOf(log, "abort:ro"); a < 0 || lastRenewBefore(log, a) != a-1 {
		t.Errorf("no renew immediately before the cancel-abort; log=%v", log)
	}
}

// A gated cancel aborts via the PIN (gate_rollout_*), not the observed identity.
func TestWatcher_CancelAbort_Gated_UsesPin(t *testing.T) {
	var log []string
	w := armedW(gatedW(mkRolloutWatch("app"))) // gated + armed → pin "ro-pinned"
	st := newFakeStore(&log, w)
	now := time.Now()
	st.cancelReq = &now
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPausedIndef)}}, log: &log}
	runTick(obs, st)

	if !contains(log, "abort:ro-pinned") || !contains(log, "mark_abort_actioned") {
		t.Fatalf("gated cancel-abort should use the pin + mark_abort_actioned; log=%v", log)
	}
}

// Cancel outranks a gate reject: even with a recorded reject, a cancel routes to the
// gate-independent abort path (disarm + mark_abort_actioned), not the gated mark_actioned.
func TestWatcher_CancelBeatsReject(t *testing.T) {
	var log []string
	w := decidedW(armedW(gatedW(mkRolloutWatch("app"))), "rejected")
	st := newFakeStore(&log, w)
	now := time.Now()
	st.cancelReq = &now
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPausedIndef)}}, log: &log}
	runTick(obs, st)

	if !contains(log, "mark_abort_actioned") || contains(log, "mark_actioned") {
		t.Fatalf("cancel must outrank reject (mark_abort_actioned, not mark_actioned); log=%v", log)
	}
}

var obsAborted = obsIdentity(deploy.RolloutState{Phase: deploy.RolloutDegraded, Aborted: true})

// STICKY cancel: once rollout_abort_actioned_at is stamped, a transient cancel-read error
// must NOT drop out of the cancel branch — a FullyPromoted snapshot must not finalize
// success on an already-canceled deploy.
func TestWatcher_CancelActioned_ReadError_DoesNotFinalizeSuccess(t *testing.T) {
	var log []string
	w := mkRolloutWatch("app")
	ts := time.Now().Add(-time.Minute)
	w.RolloutAbortActionedAt = &ts // cancel-abort already issued
	st := newFakeStore(&log, w)
	st.cancelReadErr = errors.New("db blip")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPromoted)}}, log: &log}
	runTick(obs, st)

	if len(st.finalized) != 0 {
		t.Fatalf("finalized a canceled deploy despite the cancel-read error: %v", st.finalized)
	}
}

// STICKY cancel + the rollout observed aborted → FinalizeFailed(canceled), even with the
// cancel read failing.
func TestWatcher_CancelActioned_AbortedObserved_FinalizesCanceled(t *testing.T) {
	var log []string
	w := mkRolloutWatch("app")
	ts := time.Now().Add(-time.Minute)
	w.RolloutAbortActionedAt = &ts
	st := newFakeStore(&log, w)
	st.cancelReadErr = errors.New("db blip")
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsAborted)}}, log: &log}
	runTick(obs, st)

	if st.finalized[w.DeploymentRevisionID] != store.DeployStatusFailed {
		t.Fatalf("aborted+canceled should FinalizeFailed; finalized=%v log=%v", st.finalized, log)
	}
}

// The reject path (no cancel) stays on the gated mark_actioned — the negative of the
// cancel routing above.
func TestWatcher_Reject_UsesGatedMark(t *testing.T) {
	var log []string
	w := decidedW(armedW(gatedW(mkRolloutWatch("app"))), "rejected")
	st := newFakeStore(&log, w) // cancelReq nil
	obs := &fakeObserver{byApp: map[string]obsResult{"app": {state: rolloutObs(obsPausedIndef)}}, log: &log}
	runTick(obs, st)

	if !contains(log, "abort:ro-pinned") || !contains(log, "mark_actioned") {
		t.Fatalf("reject-abort should use the pin + mark_actioned; log=%v", log)
	}
	if contains(log, "mark_abort_actioned") {
		t.Fatalf("reject-abort must NOT use the cancel-path mark_abort_actioned; log=%v", log)
	}
}
