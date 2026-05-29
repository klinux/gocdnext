package domain_test

import (
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// TestNormalizeGitURL_CanonicalisesSSHAndHTTPS locks in the cross-form
// canonicalisation. The implicit project material is created with the
// scm_source URL (often SSH because operators copy "Clone with SSH"
// from the GitHub UI), but the webhook payload always delivers the
// HTTPS clone_url — without this, the two fingerprints diverge and
// the push silently doesn't trigger a run.
func TestNormalizeGitURL_CanonicalisesSSHAndHTTPS(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// SSH ↔ HTTPS for the same repo collapse to one canonical
		// host/owner/repo string.
		{"https://github.com/octocat/hello-world.git", "github.com/octocat/hello-world"},
		{"https://github.com/octocat/hello-world", "github.com/octocat/hello-world"},
		{"git@github.com:octocat/hello-world.git", "github.com/octocat/hello-world"},
		{"git@github.com:octocat/hello-world", "github.com/octocat/hello-world"},
		{"ssh://git@github.com/octocat/hello-world.git", "github.com/octocat/hello-world"},

		// Host is case-insensitive; path stays case-sensitive
		// (some forges' paths are case-sensitive).
		{"https://GitHub.com/Octocat/Hello-World.git", "github.com/Octocat/Hello-World"},
		{"git@GITHUB.COM:Octocat/Hello-World", "github.com/Octocat/Hello-World"},

		// Trailing slash + .git suffix tolerated.
		{"https://github.com/octocat/hello-world/", "github.com/octocat/hello-world"},
		{"https://github.com/octocat/hello-world.git/", "github.com/octocat/hello-world"},

		// Whitespace tolerated.
		{"  https://github.com/octocat/hello-world.git  ", "github.com/octocat/hello-world"},

		// Self-hosted host on a non-default port survives — the
		// port is part of the canonical host so two repos on
		// different ports don't collapse.
		{"https://gitea.example.com:3000/team/api.git", "gitea.example.com:3000/team/api"},

		// HTTPS with embedded credentials — strip them, keep the host.
		{"https://user:tok@gitlab.com/team/api.git", "gitlab.com/team/api"},

		// Bare local path falls through unchanged (legacy fixtures
		// in tests still need a stable string).
		{"/tmp/local-repo", "/tmp/local-repo"},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := domain.NormalizeGitURL(tc.in)
			if got != tc.want {
				t.Errorf("NormalizeGitURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestGitFingerprint_SSHAndHTTPSAgreeForSameRepo is the integration
// test for the bug fix: an SSH-form URL and HTTPS-form URL for the
// same (repo, branch) must produce the same fingerprint, otherwise
// the webhook lookup misses the implicit material row.
func TestGitFingerprint_SSHAndHTTPSAgreeForSameRepo(t *testing.T) {
	const branch = "gocdnext-tests"
	ssh := domain.GitFingerprint("git@github.com:octocat/hello-world.git", branch)
	https := domain.GitFingerprint("https://github.com/octocat/hello-world", branch)
	if ssh != https {
		t.Errorf("SSH and HTTPS fingerprints diverge: ssh=%s https=%s", ssh, https)
	}
}

// TestGitFingerprint_DifferentBranchesStayDistinct guards against an
// over-broad normalisation accidentally collapsing branches too.
func TestGitFingerprint_DifferentBranchesStayDistinct(t *testing.T) {
	main := domain.GitFingerprint("https://github.com/octocat/hello-world", "main")
	tests := domain.GitFingerprint("https://github.com/octocat/hello-world", "gocdnext-tests")
	if main == tests {
		t.Errorf("fingerprint did not change with branch")
	}
}

// TestHTTPCloneURL_RestoresSchemeOnCanonicalInput proves the
// scheme-restoration the dispatch layer relies on: canonical
// scheme-less URLs (the storage form since v0.4.4) come out as
// clonable https://… URLs; URLs that already carry a scheme or
// the SSH shorthand pass through.
func TestHTTPCloneURL_RestoresSchemeOnCanonicalInput(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"github.com/octocat/hello-world", "https://github.com/octocat/hello-world"},
		{"gitea.example.com:3000/team/api", "https://gitea.example.com:3000/team/api"},
		// Pass-through cases — scheme already there, SSH form
		// (clones via SSH key on the agent), empty.
		{"https://github.com/octocat/hello-world", "https://github.com/octocat/hello-world"},
		{"http://internal.git/team/api", "http://internal.git/team/api"},
		{"ssh://git@github.com/octocat/hello-world", "ssh://git@github.com/octocat/hello-world"},
		{"git@github.com:octocat/hello-world", "git@github.com:octocat/hello-world"},
		{"", ""},
		{"  github.com/x/y  ", "https://github.com/x/y"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := domain.HTTPCloneURL(tc.in)
			if got != tc.want {
				t.Errorf("HTTPCloneURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestGitFingerprint_DifferentReposStayDistinct guards against host
// or owner mishandling silently collapsing unrelated repos.
func TestGitFingerprint_DifferentReposStayDistinct(t *testing.T) {
	a := domain.GitFingerprint("https://github.com/octocat/hello-world", "main")
	b := domain.GitFingerprint("https://github.com/octocat/other-repo", "main")
	if a == b {
		t.Errorf("different repos hashed to the same fingerprint")
	}
}
