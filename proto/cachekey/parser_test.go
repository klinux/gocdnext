package cachekey

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr string // substring; empty = expect success
		// On success, optional invariant checks:
		wantTokens    int  // number of function-call parts (skip if <0)
		wantHasTokens bool // expectation for HasTokens()
	}{
		// Happy paths
		{
			name:          "plain literal — backwards compat with pre-v0.4.37 keys",
			raw:           "pnpm-store-static",
			wantTokens:    0,
			wantHasTokens: false,
		},
		{
			name:          "single hash token",
			raw:           `pnpm-nm-{{ hash "pnpm-lock.yaml" }}`,
			wantTokens:    1,
			wantHasTokens: true,
		},
		{
			name:          "multiple hash tokens",
			raw:           `docker-{{ hash "Dockerfile" }}-{{ hash "go.sum" }}`,
			wantTokens:    2,
			wantHasTokens: true,
		},
		{
			name:          "glob argument",
			raw:           `pnpm-nm-{{ hash "apps/*/package.json" }}`,
			wantTokens:    1,
			wantHasTokens: true,
		},
		{
			name:          "token-only key (no literal prefix/suffix)",
			raw:           `{{ hash "go.sum" }}`,
			wantTokens:    1,
			wantHasTokens: true,
		},
		{
			name:          "extra whitespace inside token",
			raw:           `pnpm-{{   hash    "lock.yaml"   }}-x`,
			wantTokens:    1,
			wantHasTokens: true,
		},

		// Limits
		{
			name:    "empty key rejected",
			raw:     "",
			wantErr: "empty",
		},
		{
			name:    "raw length > MaxKeyLength",
			raw:     strings.Repeat("a", MaxKeyLength+1),
			wantErr: "raw template length",
		},
		{
			name:    "too many tokens",
			raw:     `{{ hash "a" }}{{ hash "b" }}{{ hash "c" }}{{ hash "d" }}{{ hash "e" }}{{ hash "f" }}`,
			wantErr: "tokens exceeds max",
		},
		{
			name:    "arg length > MaxArgLength",
			raw:     fmt.Sprintf(`x-{{ hash "%s" }}`, strings.Repeat("a", MaxArgLength+1)),
			wantErr: "argument length",
		},

		// Whitelist
		{
			name:    "unknown function",
			raw:     `pnpm-{{ env "FOO" }}`,
			wantErr: `unknown function "env"`,
		},
		{
			name:    "typo in function name",
			raw:     `pnpm-{{ haxh "lock" }}`,
			wantErr: `unknown function "haxh"`,
		},

		// Argument shape
		{
			name:    "empty argument rejected",
			raw:     `pnpm-{{ hash "" }}`,
			wantErr: "empty argument",
		},
		{
			name:    "path traversal `..` rejected",
			raw:     `x-{{ hash "../etc/passwd" }}`,
			wantErr: "must not contain `..`",
		},
		{
			name:    "deeply nested traversal rejected",
			raw:     `x-{{ hash "a/b/../../etc" }}`,
			wantErr: "must not contain `..`",
		},
		{
			name:    "absolute path rejected",
			raw:     `x-{{ hash "/etc/passwd" }}`,
			wantErr: "must be workspace-relative",
		},

		// Malformed templates
		{
			name:    "missing closing brace",
			raw:     `pnpm-{{ hash "lock"`,
			wantErr: "malformed",
		},
		{
			name:    "unmatched opening brace in literal",
			raw:     `pnpm-{{ broken without close`,
			wantErr: "malformed",
		},
		{
			name:    "stray opening brace after valid token",
			raw:     `pnpm-{{ hash "ok.txt" }}-{{ broken`,
			wantErr: "malformed",
		},
		{
			name: "arg lacks closing quote",
			raw:  `pnpm-{{ hash "lock.yaml }}`,
			// regex won't match (no closing `"`), straggler fires.
			wantErr: "malformed",
		},

		// Parse-time literal charset ONLY applies to tokenized keys.
		// Zero-token (legacy) keys preserve old opaque-string
		// behaviour — see literalChunkCharsetRE docstring for why
		// (storage hashes the raw key; legacy pipelines documented
		// `pnpm-store-${CI_BRANCH}` etc. for years).
		{
			name:    "literal slash in TOKENIZED key rejected at parse",
			raw:     `a/b-{{ hash "go.sum" }}`,
			wantErr: "outside [a-zA-Z0-9-_.]",
		},
		{
			name:          "literal slash in PURE-LITERAL key accepted (legacy compat)",
			raw:           `a/b-static`,
			wantTokens:    0,
			wantHasTokens: false,
		},
		{
			name:          "shell-substitution syntax in PURE-LITERAL key accepted (legacy compat)",
			raw:           `pnpm-store-${CI_COMMIT_BRANCH}`,
			wantTokens:    0,
			wantHasTokens: false,
		},
		{
			name:          "space in PURE-LITERAL key accepted (legacy compat — opaque string)",
			raw:           `bad key`,
			wantTokens:    0,
			wantHasTokens: false,
		},
		{
			name:    "literal `{` in tokenized key rejected",
			raw:     `pnpm-{x}-{{ hash "lock" }}`,
			wantErr: "outside [a-zA-Z0-9-_.]",
		},
		{
			name:    "literal `}` after token in tokenized key rejected",
			raw:     `pnpm-{{ hash "lock" }}-end}`,
			wantErr: "outside [a-zA-Z0-9-_.]",
		},
		{
			name:          "adjacent tokens (empty literal chunk between them is OK)",
			raw:           `{{ hash "a" }}{{ hash "b" }}`,
			wantTokens:    2,
			wantHasTokens: true,
		},
		{
			name:          "dot and underscore in literal allowed",
			raw:           `pnpm.nm_v2-{{ hash "lock" }}`,
			wantTokens:    1,
			wantHasTokens: true,
		},
		{
			name:    "shell-substitution mixed with templates is rejected",
			raw:     `pnpm-${CI_BRANCH}-{{ hash "lock" }}`,
			wantErr: "outside [a-zA-Z0-9-_.]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tpl, err := Parse(tt.raw)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse(%q) returned nil error, want substring %q", tt.raw, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Parse(%q) err = %q, want substring %q", tt.raw, err.Error(), tt.wantErr)
				}
				if tpl != nil {
					t.Errorf("Parse(%q) returned non-nil template on error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) returned %v, want nil", tt.raw, err)
			}
			if tpl == nil {
				t.Fatalf("Parse(%q) returned nil template", tt.raw)
			}
			if tpl.Raw() != tt.raw {
				t.Errorf("Raw() = %q, want %q", tpl.Raw(), tt.raw)
			}
			if tpl.HasTokens() != tt.wantHasTokens {
				t.Errorf("HasTokens() = %v, want %v", tpl.HasTokens(), tt.wantHasTokens)
			}
			// Count function parts.
			tokens := 0
			for _, p := range tpl.parts {
				if p.fn != "" {
					tokens++
				}
			}
			if tokens != tt.wantTokens {
				t.Errorf("got %d function tokens, want %d", tokens, tt.wantTokens)
			}
		})
	}
}

// fakeResolver returns a fixed hex for any arg.
type fakeResolver struct {
	out string
	err error
}

func (f fakeResolver) Hash(arg string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

// argRecordingResolver captures which args it was called with so
// the test can assert the expansion sequence.
type argRecordingResolver struct {
	seen []string
	out  string
}

func (r *argRecordingResolver) Hash(arg string) (string, error) {
	r.seen = append(r.seen, arg)
	return r.out, nil
}

func TestTemplate_Expand(t *testing.T) {
	t.Parallel()

	t.Run("plain literal is no-op", func(t *testing.T) {
		t.Parallel()
		tpl, _ := Parse("pnpm-store-static")
		got, err := tpl.Expand(fakeResolver{})
		if err != nil {
			t.Fatalf("Expand returned %v", err)
		}
		if got != "pnpm-store-static" {
			t.Errorf("got %q, want pnpm-store-static", got)
		}
	})

	t.Run("single token substituted", func(t *testing.T) {
		t.Parallel()
		tpl, _ := Parse(`pnpm-nm-{{ hash "pnpm-lock.yaml" }}`)
		got, err := tpl.Expand(fakeResolver{out: "abc123def456"})
		if err != nil {
			t.Fatalf("Expand returned %v", err)
		}
		if got != "pnpm-nm-abc123def456" {
			t.Errorf("got %q, want pnpm-nm-abc123def456", got)
		}
	})

	t.Run("multiple tokens expanded in order", func(t *testing.T) {
		t.Parallel()
		tpl, _ := Parse(`a-{{ hash "X" }}-b-{{ hash "Y" }}-c`)
		r := &argRecordingResolver{out: "deadbeef0000"}
		got, err := tpl.Expand(r)
		if err != nil {
			t.Fatalf("Expand returned %v", err)
		}
		want := "a-deadbeef0000-b-deadbeef0000-c"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if len(r.seen) != 2 || r.seen[0] != "X" || r.seen[1] != "Y" {
			t.Errorf("resolver saw args %v, want [X Y]", r.seen)
		}
	})

	t.Run("resolver error propagates with token context", func(t *testing.T) {
		t.Parallel()
		tpl, _ := Parse(`x-{{ hash "missing.yaml" }}`)
		sentinel := errors.New("glob matched 0 files")
		_, err := tpl.Expand(fakeResolver{err: sentinel})
		if err == nil {
			t.Fatalf("Expand returned nil, want error")
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("err doesn't wrap sentinel: %v", err)
		}
		if !strings.Contains(err.Error(), "missing.yaml") {
			t.Errorf("err missing arg context: %v", err)
		}
	})

	t.Run("resolver returns wrong length is caught", func(t *testing.T) {
		t.Parallel()
		tpl, _ := Parse(`x-{{ hash "ok" }}`)
		_, err := tpl.Expand(fakeResolver{out: "short"})
		if err == nil {
			t.Fatalf("Expand returned nil for wrong-length output")
		}
		if !strings.Contains(err.Error(), "hex chars") {
			t.Errorf("err = %v, want it to mention hex char count", err)
		}
	})

	t.Run("expand-time charset is defense-in-depth for buggy resolver", func(t *testing.T) {
		t.Parallel()
		// Parse-time charset now rejects bad literals (see TestParse
		// cases). The Expand-time check exists as a backstop: if a
		// future function ever returns chars outside the hex set
		// (e.g. base64-encoded output), the expanded key still
		// passes through the charset gate before reaching storage.
		// Simulate by having the fake resolver return slashes.
		tpl, err := Parse(`prefix-{{ hash "ok" }}`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		// "abc/123/4567" — wrong length AND wrong charset; the
		// length check fires first, which is the correct ordering
		// (length is cheaper to evaluate). Use a 12-char output
		// with a slash in the middle so we exercise the charset
		// branch specifically.
		_, err = tpl.Expand(fakeResolver{out: "abc1/2def456"})
		if err == nil {
			t.Fatalf("Expand returned nil for resolver returning `/`")
		}
		if !strings.Contains(err.Error(), "outside [a-zA-Z0-9-_.]") {
			t.Errorf("err = %v, want charset message", err)
		}
	})
}
