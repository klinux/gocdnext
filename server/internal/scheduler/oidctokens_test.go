package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/oidcissuer"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// idTokenRun builds a RunForDispatch whose definition declares one
// job with the given id_tokens specs.
func idTokenRun(t *testing.T, jobName string, specs map[string]domain.IDTokenSpec, cause string, causeDetail string) store.RunForDispatch {
	t.Helper()
	def := domain.Pipeline{
		Name:   "ci",
		Stages: []string{"s"},
		Jobs: []domain.Job{{
			Name: jobName, Stage: "s", Image: "alpine",
			Tasks:    []domain.Task{{Script: "true"}},
			IDTokens: specs,
		}},
	}
	blob, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), ProjectID: uuid.New(),
		ProjectSlug: "shop", Counter: 7,
		Definition: blob,
		Revisions:  json.RawMessage(`{"00000000-0000-0000-0000-000000000001": {"revision": "abc123def", "branch": "main"}}`),
		Cause:      cause,
	}
	if causeDetail != "" {
		run.CauseDetail = json.RawMessage(causeDetail)
	}
	return run
}

// TestBuildAssignment_IDTokensInEnvAndMasks — the security splice:
// the JWT must land in env under the declared name AND, verbatim,
// in LogMasks. Two tokens → two distinct JWTs.
func TestBuildAssignment_IDTokensInEnvAndMasks(t *testing.T) {
	run := idTokenRun(t, "deploy", map[string]domain.IDTokenSpec{
		"GCP_TOKEN":   {Aud: []string{"https://gcp"}},
		"VAULT_TOKEN": {Aud: []string{"https://vault"}},
	}, "webhook", "")
	job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy"}

	idTokens := map[string]string{
		"GCP_TOKEN":   "eyJhbGciOiJSUzI1NiJ9.gcp-payload.sig1",
		"VAULT_TOKEN": "eyJhbGciOiJSUzI1NiJ9.vault-payload.sig2",
	}
	got, _, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil, nil, idTokens, "")
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	for name, token := range idTokens {
		if got.Env[name] != token {
			t.Errorf("env[%s] = %q, want the minted JWT", name, got.Env[name])
		}
		var masked bool
		for _, m := range got.LogMasks {
			if m == token {
				masked = true
			}
		}
		if !masked {
			t.Errorf("JWT for %s missing from LogMasks — bearer token would leak into log streams", name)
		}
	}
}

// fakeMinter counts mints and can fail on demand.
type fakeMinter struct {
	mints atomic.Int64
	fail  bool
}

func (f *fakeMinter) Mint(_ context.Context, jc oidcissuer.JobClaims, aud []string) (string, error) {
	f.mints.Add(1)
	if f.fail {
		return "", fmt.Errorf("boom")
	}
	return "jwt-for-" + jc.Job + "-" + strings.Join(aud, ","), nil
}

// TestMintIDTokens_ViaScheduler — exercises Scheduler.mintIDTokens
// through the exported test hook: declared tokens are minted with
// the right audiences; jobs without declarations never touch the
// minter; declarations without a minter fail with the
// configuration-error message.
func TestMintIDTokens_ViaScheduler(t *testing.T) {
	ctx := context.Background()

	t.Run("declared tokens minted with the declared audiences", func(t *testing.T) {
		m := &fakeMinter{}
		s := scheduler.New(nil, nil, nil, "").WithIDTokenMinter(m)
		run := idTokenRun(t, "deploy", map[string]domain.IDTokenSpec{
			"GCP_TOKEN": {Aud: []string{"https://gcp"}},
		}, "webhook", "")
		job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy"}

		tokens, err := scheduler.MintIDTokensForTest(s, ctx, run, job)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if tokens["GCP_TOKEN"] != "jwt-for-deploy-https://gcp" {
			t.Errorf("tokens = %v", tokens)
		}
		if m.mints.Load() != 1 {
			t.Errorf("mints = %d, want 1", m.mints.Load())
		}
	})

	t.Run("job without id_tokens never touches the minter", func(t *testing.T) {
		m := &fakeMinter{}
		s := scheduler.New(nil, nil, nil, "").WithIDTokenMinter(m)
		run := idTokenRun(t, "build", nil, "webhook", "")
		job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "build"}

		tokens, err := scheduler.MintIDTokensForTest(s, ctx, run, job)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if tokens != nil {
			t.Errorf("tokens = %v, want nil", tokens)
		}
		if m.mints.Load() != 0 {
			t.Errorf("minter touched %d times for a job without id_tokens", m.mints.Load())
		}
	})

	t.Run("declaration with issuer disabled fails loud", func(t *testing.T) {
		s := scheduler.New(nil, nil, nil, "") // no minter
		run := idTokenRun(t, "deploy", map[string]domain.IDTokenSpec{
			"GCP_TOKEN": {Aud: []string{"https://gcp"}},
		}, "webhook", "")
		job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy"}

		_, err := scheduler.MintIDTokensForTest(s, ctx, run, job)
		if err == nil || !strings.Contains(err.Error(), "OIDC issuer is disabled") {
			t.Errorf("err = %v, want configuration-error message", err)
		}
	})

	t.Run("minter failure propagates with the token name", func(t *testing.T) {
		m := &fakeMinter{fail: true}
		s := scheduler.New(nil, nil, nil, "").WithIDTokenMinter(m)
		run := idTokenRun(t, "deploy", map[string]domain.IDTokenSpec{
			"GCP_TOKEN": {Aud: []string{"https://gcp"}},
		}, "webhook", "")
		job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy"}

		_, err := scheduler.MintIDTokensForTest(s, ctx, run, job)
		if err == nil || !strings.Contains(err.Error(), "GCP_TOKEN") {
			t.Errorf("err = %v, want mention of GCP_TOKEN", err)
		}
	})
}

// claimCapture records the JobClaims the scheduler hands the minter.
type claimCapture struct {
	jc oidcissuer.JobClaims
}

func (c *claimCapture) Mint(_ context.Context, jc oidcissuer.JobClaims, _ []string) (string, error) {
	c.jc = jc
	return "tok", nil
}

// TestMintIDTokens_ClaimsPerCause — the claims handed to the issuer
// must reflect the run's cause: PR runs produce the ref-less
// :pull_request sub; tag runs carry ref_type=tag; branch runs the
// branch. Matrix key rides along.
func TestMintIDTokens_ClaimsPerCause(t *testing.T) {
	ctx := context.Background()
	specs := map[string]domain.IDTokenSpec{"T": {Aud: []string{"a"}}}

	t.Run("branch run", func(t *testing.T) {
		cap := &claimCapture{}
		s := scheduler.New(nil, nil, nil, "").WithIDTokenMinter(cap)
		run := idTokenRun(t, "deploy", specs, "webhook", "")
		job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy", MatrixKey: "shard=a"}
		if _, err := scheduler.MintIDTokensForTest(s, ctx, run, job); err != nil {
			t.Fatalf("mint: %v", err)
		}
		if cap.jc.RefType != "branch" || cap.jc.Ref != "main" {
			t.Errorf("ref = %s/%s, want branch/main", cap.jc.RefType, cap.jc.Ref)
		}
		if cap.jc.SHA != "abc123def" {
			t.Errorf("sha = %q", cap.jc.SHA)
		}
		if cap.jc.MatrixKey != "shard=a" {
			t.Errorf("matrix_key = %q", cap.jc.MatrixKey)
		}
		if cap.jc.ProjectSlug != "shop" || cap.jc.Pipeline != "ci" {
			t.Errorf("identity claims = %+v", cap.jc)
		}
		if !strings.Contains(cap.jc.Subject(), ":ref_type:branch:ref:main") {
			t.Errorf("sub = %q", cap.jc.Subject())
		}
	})

	t.Run("pull_request run gets ref-less sub + pr_number", func(t *testing.T) {
		cap := &claimCapture{}
		s := scheduler.New(nil, nil, nil, "").WithIDTokenMinter(cap)
		run := idTokenRun(t, "deploy", specs, "pull_request", `{"pr_number": 42}`)
		job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy"}
		if _, err := scheduler.MintIDTokensForTest(s, ctx, run, job); err != nil {
			t.Fatalf("mint: %v", err)
		}
		if cap.jc.PRNumber != "42" {
			t.Errorf("pr_number = %q", cap.jc.PRNumber)
		}
		sub := cap.jc.Subject()
		if !strings.HasSuffix(sub, ":pull_request") || strings.Contains(sub, "ref:") {
			t.Errorf("PR sub = %q — must end :pull_request and carry NO ref segment", sub)
		}
	})

	t.Run("tag run", func(t *testing.T) {
		cap := &claimCapture{}
		s := scheduler.New(nil, nil, nil, "").WithIDTokenMinter(cap)
		run := idTokenRun(t, "deploy", specs, "tag", `{"tag_name": "v1.2.3"}`)
		job := store.DispatchableJob{ID: uuid.New(), RunID: run.ID, Name: "deploy"}
		if _, err := scheduler.MintIDTokensForTest(s, ctx, run, job); err != nil {
			t.Fatalf("mint: %v", err)
		}
		if cap.jc.RefType != "tag" || cap.jc.Ref != "v1.2.3" {
			t.Errorf("ref = %s/%s, want tag/v1.2.3", cap.jc.RefType, cap.jc.Ref)
		}
	})
}
