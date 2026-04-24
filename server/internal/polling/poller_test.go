package polling_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/polling"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// newCipher spins up a deterministic cipher for tests. scm_source
// binding requires a cipher to seal the synthesized webhook secret
// (even when the caller never reads it back) — the store refuses
// the apply without one.
func newCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

// setupStore returns a fresh pool + store with cipher wired.
// Tests that need raw SQL access use the pool; everything else
// goes through the store.
func setupStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newCipher(t))
	return s, pool
}

// fakeResolver is a thread-safe HeadResolver stub. Tests queue up
// responses (per call) so the poller sees a scripted sequence:
// first tick: sha A, second tick: sha B, etc.
type fakeResolver struct {
	mu   sync.Mutex
	sha  string
	err  error
	hits int
}

func (f *fakeResolver) HeadSHA(ctx context.Context, scm store.SCMSource, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	return f.sha, f.err
}

func (f *fakeResolver) set(sha string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sha = sha
	f.err = err
}

func (f *fakeResolver) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// TestPoller_FiresRunOnNewHead applies a pipeline with a git
// material that has poll_interval=1m, no scm_source binding, and
// no prior poll state. Since the implicit project material needs
// an scm_source, we declare the material explicitly in YAML +
// bind the project to an scm_source so the poller has the
// provider + url + auth to hit. Then we drive the ticker once and
// assert a run lands with cause="poll".
func TestPoller_FiresRunOnNewHead(t *testing.T) {
	s, pool := setupStore(t)
	ctx := context.Background()

	const repoURL = "https://github.com/org/polltest"
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "pollproj", Name: "PollProj",
		SCMSource: &store.SCMSourceInput{
			Provider: "github", URL: repoURL, DefaultBranch: "main",
		},
		Pipelines: []*domain.Pipeline{{
			Name:   "poll-pipe",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(repoURL, "main"),
				AutoUpdate:  true,
				Git: &domain.GitMaterial{
					URL:          repoURL,
					Branch:       "main",
					Events:       []string{"push"},
					PollInterval: time.Minute,
				},
			}},
			Jobs: []domain.Job{{
				Name: "run", Stage: "build", Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo hi"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	resolver := &fakeResolver{}
	resolver.set("deadbeefcafe", nil)

	ticker := polling.New(s, resolver,
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithTick(50 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ticker.Run(runCtx) }()

	// Wait for the poll-caused run to materialize.
	deadline := time.Now().Add(3 * time.Second)
	var runCount int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM runs WHERE pipeline_id = $1 AND cause = 'poll'`,
			pipelineID,
		).Scan(&runCount); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		if runCount > 0 {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	cancel()
	<-done

	if runCount == 0 {
		t.Fatalf("expected at least one poll-caused run; got 0. resolver calls=%d", resolver.calls())
	}

	// Poll-state must record the sha.
	var sha string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(ps.last_head_sha, '') FROM material_poll_state ps
		JOIN materials m ON m.id = ps.material_id
		WHERE m.pipeline_id = $1`, pipelineID,
	).Scan(&sha); err != nil {
		t.Fatalf("poll_state lookup: %v", err)
	}
	if sha != "deadbeefcafe" {
		t.Errorf("last_head_sha: want deadbeefcafe, got %q", sha)
	}
}

// TestPoller_IdempotentSameSHA drives two evaluates against the
// same sha: the second must not create a second run. Verifies
// that ON CONFLICT on the modification unique key holds when the
// poll sees HEAD unchanged.
func TestPoller_IdempotentSameSHA(t *testing.T) {
	s, pool := setupStore(t)
	ctx := context.Background()

	const repoURL = "https://github.com/org/polltest2"
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "pollproj2", Name: "PollProj2",
		SCMSource: &store.SCMSourceInput{
			Provider: "github", URL: repoURL, DefaultBranch: "main",
		},
		Pipelines: []*domain.Pipeline{{
			Name:   "pipe2",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(repoURL, "main"),
				AutoUpdate:  true,
				Git: &domain.GitMaterial{
					URL:          repoURL,
					Branch:       "main",
					Events:       []string{"push"},
					PollInterval: time.Minute,
				},
			}},
			Jobs: []domain.Job{{
				Name: "run", Stage: "build", Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo hi"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	resolver := &fakeResolver{}
	resolver.set("sha-stable", nil)

	ticker := polling.New(s, resolver,
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithTick(20 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ticker.Run(runCtx) }()

	// Give the ticker enough time for multiple evaluates on the
	// same sha. Without the IsDue gate, a 20ms tick over 300ms
	// would fire ~15 runs; with the gate + poll_interval=1m, only
	// the first qualifies (LastPolledAt==nil) and subsequent ticks
	// are blocked by the 1m interval. We expect exactly ONE run.
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	var runCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs WHERE pipeline_id = $1 AND cause = 'poll'`,
		pipelineID,
	).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 1 {
		t.Errorf("expected exactly 1 poll run (stable sha + 1m interval gates re-fires); got %d", runCount)
	}
}

// TestPoller_ResolverErrorRecordsOutcome fails the resolver so
// the poller records a last_poll_error instead of crashing or
// looping. Verifies the UI signal path stays alive under provider
// outages.
func TestPoller_ResolverErrorRecordsOutcome(t *testing.T) {
	s, pool := setupStore(t)
	ctx := context.Background()

	const repoURL = "https://github.com/org/pollfail"
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "pollfail", Name: "PollFail",
		SCMSource: &store.SCMSourceInput{
			Provider: "github", URL: repoURL, DefaultBranch: "main",
		},
		Pipelines: []*domain.Pipeline{{
			Name:   "pf",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(repoURL, "main"),
				AutoUpdate:  true,
				Git: &domain.GitMaterial{
					URL:          repoURL,
					Branch:       "main",
					Events:       []string{"push"},
					PollInterval: time.Minute,
				},
			}},
			Jobs: []domain.Job{{
				Name: "run", Stage: "build", Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo hi"}},
			}},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	resolver := &fakeResolver{}
	resolver.set("", errors.New("boom: auth failed"))

	ticker := polling.New(s, resolver,
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithTick(30 * time.Millisecond)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ticker.Run(runCtx) }()

	deadline := time.Now().Add(2 * time.Second)
	var errMsg string
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(last_poll_error, '') FROM material_poll_state`,
		).Scan(&errMsg); err != nil {
			// No row yet — keep polling.
			time.Sleep(30 * time.Millisecond)
			continue
		}
		if errMsg != "" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	<-done

	if errMsg == "" {
		t.Fatalf("expected last_poll_error to be set; got empty")
	}
}
