package deploysvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

type fakeSyncerSvc struct {
	called bool
	gotRev string
	err    error
}

func (f *fakeSyncerSvc) Sync(_ context.Context, _ deploy.DeploymentTarget, revision string) error {
	f.called = true
	f.gotRev = revision
	return f.err
}

type fakeNativeStore struct {
	target     store.DeployTarget
	resolveErr error
	startRes   store.StartNativeDeployResult
	startErr   error
	stampOK    bool
	stampErr   error

	startCalled bool
	stampCalled bool
	gotStartIn  store.StartNativeDeployInput
}

func (f *fakeNativeStore) ResolveDeployTarget(_ context.Context, _ uuid.UUID, _ string) (store.DeployTarget, error) {
	return f.target, f.resolveErr
}

func (f *fakeNativeStore) StartNativeDeploy(_ context.Context, in store.StartNativeDeployInput) (store.StartNativeDeployResult, error) {
	f.startCalled = true
	f.gotStartIn = in
	return f.startRes, f.startErr
}

func (f *fakeNativeStore) StampDeployWatchSyncRequested(_ context.Context, _ uuid.UUID) (bool, error) {
	f.stampCalled = true
	return f.stampOK, f.stampErr
}

func triggerTarget() store.DeployTarget {
	return store.DeployTarget{
		ProjectID: uuid.New(), EnvironmentID: uuid.New(), Environment: "production", Provider: "argocd",
		Cluster: "prod", Application: "checkout", Namespace: "argocd", SyncMode: "trigger",
	}
}

func input() NativeDeployInput {
	return NativeDeployInput{
		ProjectID: uuid.New(), RunID: uuid.New(), JobRunID: uuid.New(),
		Environment: "production", Version: "v1.2.3",
		Revision:   "abc0123456789abc0123456789abc0123456789a", // full SHA
		DeployedBy: "svc", Now: time.Now(),
	}
}

func newDeployer(sync Syncer, st NativeStore) *NativeDeployer {
	return NewNativeDeployer(sync, st, testLogger())
}

func TestTakeOver_NoTargetFallsBackToPlugin(t *testing.T) {
	sync := &fakeSyncerSvc{}
	st := &fakeNativeStore{resolveErr: store.ErrDeployTargetNotFound}
	res, err := newDeployer(sync, st).TakeOver(context.Background(), input())
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if res.Decision != DecisionFallback {
		t.Fatalf("decision = %q, want fallback", res.Decision)
	}
	if st.startCalled || sync.called {
		t.Error("fallback must not start a native deploy or sync")
	}
}

func TestTakeOver_ResolveErrorFailsClosed(t *testing.T) {
	st := &fakeNativeStore{resolveErr: errors.New("db down")}
	_, err := newDeployer(&fakeSyncerSvc{}, st).TakeOver(context.Background(), input())
	if err == nil {
		t.Fatal("a real resolve error must fail closed (return an error), not fall back")
	}
	if st.startCalled {
		t.Error("must not start a deploy after a fail-closed resolve")
	}
}

func TestTakeOver_TriggerSuccess_SyncsAndStamps(t *testing.T) {
	sync := &fakeSyncerSvc{}
	revID := uuid.New()
	st := &fakeNativeStore{
		target:   triggerTarget(),
		startRes: store.StartNativeDeployResult{Started: true, RevisionID: revID, Attempt: 2},
		stampOK:  true,
	}
	res, err := newDeployer(sync, st).TakeOver(context.Background(), input())
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if res.Decision != DecisionNative || res.RevisionID != revID || res.Attempt != 2 {
		t.Fatalf("res = %+v, want native takeover", res)
	}
	// The FULL SHA (Revision) is what gets synced + correlated — never the display Version.
	if !sync.called || sync.gotRev != "abc0123456789abc0123456789abc0123456789a" {
		t.Errorf("sync called=%v rev=%q, want the full-SHA Revision synced (not the display Version)", sync.called, sync.gotRev)
	}
	if st.gotStartIn.ExpectedRevision != "abc0123456789abc0123456789abc0123456789a" || st.gotStartIn.Version != "v1.2.3" {
		t.Errorf("start input: ExpectedRevision=%q Version=%q, want full-SHA correlation + semver display",
			st.gotStartIn.ExpectedRevision, st.gotStartIn.Version)
	}
	if !st.stampCalled {
		t.Error("trigger success must stamp sync_requested_at")
	}
	// The start input carried the resolved target + env id + a future deadline.
	if st.gotStartIn.EnvironmentID != st.target.EnvironmentID || st.gotStartIn.Application != "checkout" {
		t.Errorf("start input not built from the resolved target: %+v", st.gotStartIn)
	}
	if !st.gotStartIn.DeadlineAt.After(time.Now()) {
		t.Errorf("deadline %v is not in the future", st.gotStartIn.DeadlineAt)
	}
}

func TestTakeOver_ObserveMode_NoSyncNoStamp(t *testing.T) {
	sync := &fakeSyncerSvc{}
	tgt := triggerTarget()
	tgt.SyncMode = "observe"
	st := &fakeNativeStore{target: tgt, startRes: store.StartNativeDeployResult{Started: true, RevisionID: uuid.New()}}
	res, err := newDeployer(sync, st).TakeOver(context.Background(), input())
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if res.Decision != DecisionNative {
		t.Fatalf("decision = %q, want native", res.Decision)
	}
	if sync.called || st.stampCalled {
		t.Error("observe mode must neither sync nor stamp")
	}
}

// A Sync failure does NOT fail the takeover or complete the job — the watcher
// deadline-fails (single terminalizer). No stamp (no correlation anchor).
func TestTakeOver_TriggerSyncFails_NoStamp_StillNative(t *testing.T) {
	sync := &fakeSyncerSvc{err: errors.New("patch rejected")}
	st := &fakeNativeStore{
		target:   triggerTarget(),
		startRes: store.StartNativeDeployResult{Started: true, RevisionID: uuid.New()},
	}
	res, err := newDeployer(sync, st).TakeOver(context.Background(), input())
	if err != nil {
		t.Fatalf("a sync failure must not fail the takeover: %v", err)
	}
	if res.Decision != DecisionNative {
		t.Fatalf("decision = %q, want native (job already server-managed)", res.Decision)
	}
	if st.stampCalled {
		t.Error("a failed sync must NOT stamp the correlation anchor")
	}
}

func TestTakeOver_LostCAS_Skips(t *testing.T) {
	sync := &fakeSyncerSvc{}
	st := &fakeNativeStore{target: triggerTarget(), startRes: store.StartNativeDeployResult{Started: false}}
	res, err := newDeployer(sync, st).TakeOver(context.Background(), input())
	if err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if res.Decision != DecisionSkip {
		t.Fatalf("decision = %q, want skip (lost CAS)", res.Decision)
	}
	if sync.called {
		t.Error("a lost CAS must not sync")
	}
}

func TestTakeOver_StartErrorFailsClosed(t *testing.T) {
	st := &fakeNativeStore{target: triggerTarget(), startErr: errors.New("tx failed")}
	_, err := newDeployer(&fakeSyncerSvc{}, st).TakeOver(context.Background(), input())
	if err == nil {
		t.Fatal("a StartNativeDeploy error must fail closed")
	}
}
