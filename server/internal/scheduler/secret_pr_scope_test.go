package scheduler_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// A pull_request-caused run must be refused a declared secret that isn't exposed
// to PRs, and allowed one that is; a non-PR run ignores the flag entirely. This
// is the #223 PR-head-config follow-up: a contributor who controls a PR branch
// could otherwise edit the pipeline to exfiltrate a secret.
func TestResolveJobSecrets_PullRequestExposureGate(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	resolver, err := secrets.NewDBResolver(s, cipher)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	sched := scheduler.New(s, grpcsrv.NewSessionStore(), quietLogger(), "").
		WithSecretResolver(resolver)

	ctx := context.Background()
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "demo"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID := applied.ProjectID

	// CLOSED is PR-withheld (default for new secrets); OPEN is opted in.
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: projectID, Name: "CLOSED", Value: []byte("closed-val"), ExposeOnPR: false,
	}); err != nil {
		t.Fatalf("set CLOSED: %v", err)
	}
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: projectID, Name: "OPEN", Value: []byte("open-val"), ExposeOnPR: true,
	}); err != nil {
		t.Fatalf("set OPEN: %v", err)
	}

	defWith := func(names ...string) []byte {
		b, mErr := json.Marshal(domain.Pipeline{
			Stages: []string{"build"},
			Jobs:   []domain.Job{{Name: "job", Stage: "build", Secrets: names}},
		})
		if mErr != nil {
			t.Fatalf("marshal def: %v", mErr)
		}
		return b
	}

	// 1. PR run declaring the PR-closed secret → refused, naming it, and the value
	//    is never returned.
	_, err = scheduler.ResolveJobSecretsForTest(sched, ctx,
		store.RunForDispatch{ProjectID: projectID, Definition: defWith("CLOSED"), Cause: string(domain.CausePullRequest)},
		"job")
	if err == nil {
		t.Fatal("PR run with a PR-closed secret should be refused, got nil")
	}
	if !strings.Contains(err.Error(), "pull_request") || !strings.Contains(err.Error(), "CLOSED") {
		t.Fatalf("error should name the withheld secret + pull_request, got: %v", err)
	}

	// 2. PR run declaring only the PR-exposed secret → resolves.
	got, err := scheduler.ResolveJobSecretsForTest(sched, ctx,
		store.RunForDispatch{ProjectID: projectID, Definition: defWith("OPEN"), Cause: string(domain.CausePullRequest)},
		"job")
	if err != nil {
		t.Fatalf("PR run with a PR-exposed secret should resolve, got: %v", err)
	}
	if got["OPEN"] != "open-val" {
		t.Fatalf("OPEN = %q, want open-val", got["OPEN"])
	}

	// 3. A non-PR run ignores the flag — the PR-closed secret resolves normally.
	got, err = scheduler.ResolveJobSecretsForTest(sched, ctx,
		store.RunForDispatch{ProjectID: projectID, Definition: defWith("CLOSED"), Cause: "push"},
		"job")
	if err != nil {
		t.Fatalf("non-PR run should resolve regardless of the PR flag, got: %v", err)
	}
	if got["CLOSED"] != "closed-val" {
		t.Fatalf("CLOSED on push = %q, want closed-val", got["CLOSED"])
	}
}
