package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedWatchable sets up project → cluster → env → an in_progress deployment
// revision, and returns (projectID, revisionID) ready for a deploy_watch.
func seedWatchable(t *testing.T, s *store.Store, ctx context.Context, slug string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	projectID := seedProject(t, s, slug)
	if _, err := s.InsertCluster(ctx, newAuthCipher(t), store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure environment: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Attempt: 0, Version: "v1", DeployedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	return projectID, revID
}

func newWatchInput(projectID, revID uuid.UUID) store.DeployWatchInput {
	return store.DeployWatchInput{
		DeploymentRevisionID: revID,
		ProjectID:            projectID,
		SyncMode:             "trigger",
		Cluster:              "prod-gke",
		Application:          "checkout",
		Namespace:            "argocd",
		ExpectedRevision:     "abc123",
		DeadlineAt:           time.Now().Add(10 * time.Minute),
	}
}

// The full watcher lifecycle plus the fencing guarantee: a watcher whose lease was
// reclaimed by another replica can neither renew nor terminalize the deploy.
func TestDeployWatch_ClaimRenewFinalize_Fencing(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-life")

	w, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID))
	if err != nil {
		t.Fatalf("create watch: %v", err)
	}
	if w.ClaimID != uuid.Nil || w.SyncRequestedAt != nil || w.ExpectedRevision != "abc123" {
		t.Fatalf("fresh watch not unclaimed/pre-sync: %+v", w)
	}

	// Claim it (worker1) → gets a fencing token.
	claimed, err := s.ClaimDeployWatches(ctx, "worker1", 3600, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].DeploymentRevisionID != revID || claimed[0].ClaimID == uuid.Nil {
		t.Fatalf("claim = %+v, want the one watch with a token", claimed)
	}
	c1 := claimed[0].ClaimID

	// A fresh lease is NOT reclaimable by another replica.
	if again, err := s.ClaimDeployWatches(ctx, "worker2", 3600, 10); err != nil || len(again) != 0 {
		t.Fatalf("re-claim of a fresh lease = %v (err %v), want none", again, err)
	}

	// Correlation anchor + heartbeat under the held token.
	if ok, err := s.MarkDeployWatchSyncRequested(ctx, revID, c1); err != nil || !ok {
		t.Fatalf("mark sync-requested (held token): ok=%v err=%v", ok, err)
	}
	if ok, err := s.RenewDeployWatch(ctx, revID, c1); err != nil || !ok {
		t.Fatalf("renew (held token): ok=%v err=%v", ok, err)
	}

	// Simulate a takeover: a negative lease makes even a fresh claim reclaimable, so
	// worker2 steals it with a NEW token — no wall-clock sleep needed.
	stolen, err := s.ClaimDeployWatches(ctx, "worker2", -1, 10)
	if err != nil || len(stolen) != 1 {
		t.Fatalf("takeover claim = %v (err %v), want 1", stolen, err)
	}
	c2 := stolen[0].ClaimID
	if c2 == c1 {
		t.Fatalf("takeover reused the old token %v", c1)
	}

	// The old watcher is fenced out of BOTH renew and finalize.
	if ok, err := s.RenewDeployWatch(ctx, revID, c1); err != nil || ok {
		t.Fatalf("stale renew: ok=%v err=%v, want ok=false", ok, err)
	}
	if fin, err := s.FinalizeDeployWatch(ctx, revID, c1, "success"); err != nil || fin {
		t.Fatalf("stale finalize: finalized=%v err=%v, want finalized=false", fin, err)
	}
	// The stale finalize must NOT have terminalized the deploy or removed the watch.
	if got, err := s.GetDeployWatch(ctx, revID); err != nil {
		t.Fatalf("watch gone after a fenced-out finalize: %v", err)
	} else if got.ClaimID != c2 {
		t.Fatalf("watch token = %v, want the live token %v", got.ClaimID, c2)
	}

	// The live watcher terminalizes atomically: revision → success, watch removed.
	if fin, err := s.FinalizeDeployWatch(ctx, revID, c2, "success"); err != nil || !fin {
		t.Fatalf("live finalize: finalized=%v err=%v, want true", fin, err)
	}
	if _, err := s.GetDeployWatch(ctx, revID); err != store.ErrDeployWatchNotFound {
		t.Fatalf("GetDeployWatch after finalize = %v, want ErrDeployWatchNotFound", err)
	}
	rev, err := s.GetDeploymentRevision(ctx, revID)
	if err != nil {
		t.Fatalf("get revision: %v", err)
	}
	if rev.Status != "success" || rev.FinishedAt == nil {
		t.Fatalf("revision after finalize = %+v, want success + finished_at set", rev)
	}
}

// Degraded debounce anchor: opens on the first Degraded tick (earliest wins), clears
// on recovery — both fenced on the claim token.
func TestDeployWatch_DegradedDebounceToggle(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-degraded")
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (err %v)", claimed, err)
	}
	c := claimed[0].ClaimID

	if ok, err := s.SetDeployWatchDegradedSince(ctx, revID, c); err != nil || !ok {
		t.Fatalf("set degraded: ok=%v err=%v", ok, err)
	}
	w, _ := s.GetDeployWatch(ctx, revID)
	if w.DegradedSince == nil {
		t.Fatal("degraded_since not set")
	}
	first := *w.DegradedSince

	// A second Set keeps the earliest anchor (COALESCE), not a fresh one.
	if _, err := s.SetDeployWatchDegradedSince(ctx, revID, c); err != nil {
		t.Fatalf("set degraded again: %v", err)
	}
	w, _ = s.GetDeployWatch(ctx, revID)
	if !w.DegradedSince.Equal(first) {
		t.Fatalf("degraded_since moved: %v -> %v (want stable)", first, *w.DegradedSince)
	}

	if ok, err := s.ClearDeployWatchDegraded(ctx, revID, c); err != nil || !ok {
		t.Fatalf("clear degraded: ok=%v err=%v", ok, err)
	}
	w, _ = s.GetDeployWatch(ctx, revID)
	if w.DegradedSince != nil {
		t.Fatalf("degraded_since not cleared: %v", *w.DegradedSince)
	}

	// A stale token can't touch the debounce state.
	if ok, _ := s.SetDeployWatchDegradedSince(ctx, revID, uuid.New()); ok {
		t.Fatal("stale token set degraded, want fenced out")
	}
}

// An in-flight watch counts toward the cluster delete-guard (also FK-RESTRICTed).
func TestDeployWatch_CountActiveForCluster(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-count")

	if n, err := s.CountActiveWatchesForCluster(ctx, "prod-gke"); err != nil || n != 0 {
		t.Fatalf("count before = %d (err %v), want 0", n, err)
	}
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	if n, err := s.CountActiveWatchesForCluster(ctx, "prod-gke"); err != nil || n != 1 {
		t.Fatalf("count after = %d (err %v), want 1", n, err)
	}
}

// A revision terminalized by the JOB/reaper path (not the watcher's own
// FinalizeDeployWatch) must still delete its watch atomically — otherwise the watch
// lingers in the live queue forever and falsely blocks deleting its cluster.
func TestFinalizeDeploymentRevision_DeletesWatch(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := newAuthCipher(t)
	s.SetAuthCipher(cipher)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	if _, err := s.InsertCluster(ctx, cipher, store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure environment: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "v1", DeployedBy: "alice",
	})
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	if n, _ := s.CountActiveWatchesForCluster(ctx, "prod-gke"); n != 1 {
		t.Fatalf("precondition watches = %d, want 1", n)
	}

	// Terminalize via the JOB path.
	if n, err := s.FinalizeDeploymentRevision(ctx, jobID, 0, store.DeployStatusSuccess); err != nil || n != 1 {
		t.Fatalf("finalize by job = %d (err %v), want 1", n, err)
	}
	// The watch is atomically gone — not orphaned in the live queue or the count.
	if _, err := s.GetDeployWatch(ctx, revID); err != store.ErrDeployWatchNotFound {
		t.Fatalf("watch after job-finalize = %v, want ErrDeployWatchNotFound", err)
	}
	if n, _ := s.CountActiveWatchesForCluster(ctx, "prod-gke"); n != 0 {
		t.Fatalf("watches after job-finalize = %d, want 0 (orphan cleaned)", n)
	}
}

// CreateDeployWatch refuses to watch an already-terminal revision (a late/duplicate
// create): there is nothing to observe.
func TestCreateDeployWatch_RejectsTerminalRevision(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-terminal")

	// Drive the revision terminal through a full watch cycle.
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (err %v)", claimed, err)
	}
	if fin, err := s.FinalizeDeployWatch(ctx, revID, claimed[0].ClaimID, "success"); err != nil || !fin {
		t.Fatalf("finalize: fin=%v err=%v", fin, err)
	}

	// The revision is terminal now → a new watch is refused.
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != store.ErrRevisionNotInProgress {
		t.Fatalf("create watch on terminal revision = %v, want ErrRevisionNotInProgress", err)
	}
}

// FinalizeDeployWatch validates status up front (mirrors FinalizeDeploymentRevision),
// leaving the watch untouched rather than aborting on the DB status CHECK.
func TestFinalizeDeployWatch_InvalidStatus(t *testing.T) {
	s, ctx := newClusterStore(t)
	projectID, revID := seedWatchable(t, s, ctx, "watch-badstatus")
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (err %v)", claimed, err)
	}
	if _, err := s.FinalizeDeployWatch(ctx, revID, claimed[0].ClaimID, "canceled"); err == nil {
		t.Fatal("finalize with invalid status = nil error, want a validation error")
	}
	// The watch must survive (validation happened before any DB effect).
	if _, err := s.GetDeployWatch(ctx, revID); err != nil {
		t.Fatalf("watch gone after an invalid-status finalize: %v", err)
	}
}
