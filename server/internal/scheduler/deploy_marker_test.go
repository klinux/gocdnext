package scheduler

import (
	"errors"
	"testing"
)

func TestCorrelationRevision(t *testing.T) {
	const full = "aaa0123456789aaa0123456789aaa0123456789a" // 40 hex, the run's commit
	const other = "bbb0123456789bbb0123456789bbb0123456789b"

	tests := []struct {
		name      string
		version   string // the RESOLVED display version (omitted => the short sha)
		revision  string // the RESOLVED deploy.revision
		runCommit string // CI_COMMIT_SHA
		want      string
		wantErr   bool
	}{
		// --- rule 3: a non-SHA version is a label, not an anchor ------------------
		// These two used to be terminal. They are the whole point of the change: a
		// release label must not decide whether a native deploy can run.
		{name: "semver version falls back to the run's commit", version: "1.2.3", runCommit: full, want: full},
		{name: "GoCD-style label falls back to the run's commit", version: "1.27.aaa0123", runCommit: full, want: full},
		{name: "tag version falls back to the run's commit", version: "v1.0.0", runCommit: full, want: full},

		// --- rule 2: a SHA-shaped version keeps its strict pin semantics ----------
		{name: "full SHA version is used directly", version: full, runCommit: full, want: full},
		{name: "full SHA of a DIFFERENT commit is honored", version: other, runCommit: full, want: other},
		{name: "uppercase full SHA is lowercased", version: "AAA0123456789AAA0123456789AAA0123456789A", runCommit: full, want: full},
		{name: "short SHA prefixing the commit expands to full", version: "aaa0123", runCommit: full, want: full},
		{name: "short SHA NOT prefixing the commit stays terminal", version: "deadbee", runCommit: full, wantErr: true},
		{name: "full SHA version with no run commit still correlates", version: full, runCommit: "", want: full},
		{name: "short SHA version with no run commit cannot expand", version: "aaa0123", runCommit: "", wantErr: true},

		// --- rule 1: deploy.revision is the explicit anchor and wins --------------
		{name: "revision wins over a non-SHA version", version: "1.27.aaa0123", revision: other, runCommit: full, want: other},
		{name: "revision wins over a SHA-shaped version", version: full, revision: other, runCommit: full, want: other},
		{name: "short revision prefixing the commit expands", version: "1.2.3", revision: "aaa0123", runCommit: full, want: full},
		{name: "non-SHA revision is terminal", version: "1.2.3", revision: "v9", runCommit: full, wantErr: true},
		{name: "full revision anchors even with no usable run commit", version: "1.2.3", revision: other, runCommit: "", want: other},

		// --- the post-condition: never an empty or unmatchable anchor -------------
		// Empty would disable correlation entirely (Evaluate accepts any Synced+Healthy);
		// a non-SHA run commit would pin the watch to something ArgoCD can never report,
		// stalling until the deadline instead of failing clearly.
		{name: "non-SHA version with no run commit is terminal", version: "1.2.3", runCommit: "", wantErr: true},
		{name: "non-SHA version with a NON-SHA run commit is terminal", version: "1.2.3", runCommit: "v1.4.2", wantErr: true},
		{name: "non-SHA version with a UUID run commit is terminal", version: "1.2.3", runCommit: "3f2a1c9e-77b1-4a0e-9a8e-2a5d1f0c8b44", wantErr: true},

		// TEETH (mandatory): an OMITTED version resolves to the short sha, so it must
		// obey the same post-condition. Restoring the old `if !explicit { return
		// fullSHA }` short-circuit makes this case pass silently — and produce a watch
		// pinned to a revision ArgoCD never reports.
		{name: "omitted version (short sha) with a NON-SHA run commit is terminal", version: "aaa0123", runCommit: "release-42", wantErr: true},
		{name: "omitted version (short sha) with a full run commit correlates", version: "aaa0123", runCommit: full, want: full},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := correlationRevision("ship", tt.version, tt.revision, tt.runCommit)
			if tt.wantErr {
				if !errors.Is(err, ErrDeployRevisionNotCorrelatable) {
					t.Fatalf("err = %v, want ErrDeployRevisionNotCorrelatable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("correlationRevision = %q, want %q", got, tt.want)
			}
		})
	}
}

// The anchor is never empty on a success path — an empty ExpectedRevision makes
// deploy.Evaluate skip the revision check and accept ANY Synced+Healthy state, which is
// the stale-deploy hazard the terminal error exists to prevent.
func TestCorrelationRevision_NeverReturnsEmptyAnchor(t *testing.T) {
	const full = "aaa0123456789aaa0123456789aaa0123456789a"
	cases := []struct{ version, revision, run string }{
		{"1.2.3", "", full},
		{full, "", ""},
		{"1.2.3", full, ""},
		{"aaa0123", "", full},
	}
	for _, c := range cases {
		got, err := correlationRevision("ship", c.version, c.revision, c.run)
		if err != nil {
			continue // terminal is a valid outcome; empty-with-nil-error is not
		}
		if got == "" {
			t.Fatalf("version=%q revision=%q run=%q returned an EMPTY anchor with no error", c.version, c.revision, c.run)
		}
	}
}
