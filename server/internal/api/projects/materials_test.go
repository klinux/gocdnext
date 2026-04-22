package projects

import (
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestInjectImplicitProjectMaterial_AddsWhenAbsent(t *testing.T) {
	p := &domain.Pipeline{
		Name:      "ci-web",
		Materials: nil, // YAML didn't declare any
	}
	scm := &store.SCMSourceInput{
		Provider:      "github",
		URL:           "https://github.com/klinux/gocdnext",
		DefaultBranch: "main",
	}
	injectImplicitProjectMaterial([]*domain.Pipeline{p}, scm)

	if len(p.Materials) != 1 {
		t.Fatalf("expected 1 material, got %d", len(p.Materials))
	}
	got := p.Materials[0]
	if got.Type != domain.MaterialGit {
		t.Errorf("type = %s, want git", got.Type)
	}
	if got.Git == nil || got.Git.URL != scm.URL {
		t.Errorf("git url mismatch: %+v", got.Git)
	}
	if got.Git.Branch != "main" {
		t.Errorf("branch = %q, want main", got.Git.Branch)
	}
	if !got.Git.AutoRegisterWebhook {
		t.Errorf("auto_register_webhook should be true on implicit material")
	}
	if got.Fingerprint != domain.GitFingerprint(scm.URL, "main") {
		t.Errorf("fingerprint mismatch: %q", got.Fingerprint)
	}
	if len(got.Git.Events) != 1 || got.Git.Events[0] != "push" {
		t.Errorf("events default should be [push], got %v", got.Git.Events)
	}
}

func TestInjectImplicitProjectMaterial_HonoursTriggerEvents(t *testing.T) {
	// Top-level YAML `when.event: [push, pull_request]` propagates to
	// domain.Pipeline.TriggerEvents — the helper should feed that
	// list straight into the implicit material so PRs trigger runs
	// without the operator re-declaring the git material.
	p := &domain.Pipeline{
		Name:          "ci-server",
		TriggerEvents: []string{"push", "pull_request"},
	}
	injectImplicitProjectMaterial([]*domain.Pipeline{p}, &store.SCMSourceInput{
		Provider: "github", URL: "https://github.com/klinux/gocdnext", DefaultBranch: "main",
	})
	if len(p.Materials) != 1 {
		t.Fatalf("want 1 material, got %d", len(p.Materials))
	}
	ev := p.Materials[0].Git.Events
	if len(ev) != 2 || ev[0] != "push" || ev[1] != "pull_request" {
		t.Errorf("events = %v, want [push pull_request]", ev)
	}
}

func TestInjectImplicitProjectMaterial_SkipsWhenExplicitSameURL(t *testing.T) {
	// Operator already wrote `git: url: .../gocdnext` explicitly —
	// honour that, don't add a duplicate row. The match is on
	// normalized URL (trailing slash, .git suffix don't matter).
	explicitURL := "https://github.com/klinux/gocdnext.git/"
	scmURL := "https://github.com/klinux/gocdnext"
	p := &domain.Pipeline{
		Name: "ci-server",
		Materials: []domain.Material{{
			Type:        domain.MaterialGit,
			Fingerprint: domain.GitFingerprint(explicitURL, "main"),
			Git: &domain.GitMaterial{
				URL:    explicitURL,
				Branch: "main",
				Events: []string{"push"},
			},
		}},
	}
	before := len(p.Materials)
	injectImplicitProjectMaterial([]*domain.Pipeline{p}, &store.SCMSourceInput{
		Provider: "github", URL: scmURL, DefaultBranch: "main",
	})
	if len(p.Materials) != before {
		t.Fatalf("expected %d materials, got %d (duplicate injected?)", before, len(p.Materials))
	}
}

func TestInjectImplicitProjectMaterial_RidesAlongsideExtras(t *testing.T) {
	// ci-web declares an upstream material and a template git repo —
	// neither matches the project's scm URL, so the implicit material
	// should be appended alongside them.
	templateURL := "https://github.com/org/go-templates"
	scmURL := "https://github.com/klinux/gocdnext"
	p := &domain.Pipeline{
		Name: "ci-web",
		Materials: []domain.Material{
			{
				Type:        domain.MaterialUpstream,
				Fingerprint: domain.UpstreamFingerprint("ci-server", "test"),
				Upstream:    &domain.UpstreamMaterial{Pipeline: "ci-server", Stage: "test"},
			},
			{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(templateURL, "main"),
				Git:         &domain.GitMaterial{URL: templateURL, Branch: "main"},
			},
		},
	}
	injectImplicitProjectMaterial([]*domain.Pipeline{p}, &store.SCMSourceInput{
		Provider: "github", URL: scmURL, DefaultBranch: "main",
	})
	if len(p.Materials) != 3 {
		t.Fatalf("want 3 materials (upstream + template + implicit), got %d", len(p.Materials))
	}
	// Implicit should be last (appended).
	last := p.Materials[len(p.Materials)-1]
	if last.Git == nil || last.Git.URL != scmURL {
		t.Errorf("last material should be implicit %s, got %+v", scmURL, last.Git)
	}
}

func TestInjectImplicitProjectMaterial_NoopWhenScmNil(t *testing.T) {
	p := &domain.Pipeline{Name: "legacy"}
	injectImplicitProjectMaterial([]*domain.Pipeline{p}, nil)
	if len(p.Materials) != 0 {
		t.Fatalf("no scm → no injection; got %d materials", len(p.Materials))
	}
}

func TestInjectImplicitProjectMaterial_NoopWhenScmURLEmpty(t *testing.T) {
	p := &domain.Pipeline{Name: "legacy"}
	injectImplicitProjectMaterial([]*domain.Pipeline{p}, &store.SCMSourceInput{Provider: "github"})
	if len(p.Materials) != 0 {
		t.Fatalf("empty scm url → no injection; got %d materials", len(p.Materials))
	}
}

func TestInjectImplicitProjectMaterial_DefaultsBranchToMain(t *testing.T) {
	p := &domain.Pipeline{Name: "no-branch"}
	injectImplicitProjectMaterial([]*domain.Pipeline{p}, &store.SCMSourceInput{
		Provider: "github", URL: "https://github.com/klinux/gocdnext",
		// DefaultBranch intentionally empty — caller shouldn't have
		// to know the default before binding.
	})
	if len(p.Materials) != 1 || p.Materials[0].Git.Branch != "main" {
		t.Fatalf("branch should default to main, got %+v", p.Materials)
	}
}
