package scheduler

import (
	"strings"
	"testing"
)

func TestNeedsSatisfied(t *testing.T) {
	t.Parallel()

	row := func(matrixKey, status string) JobStatusRow {
		return JobStatusRow{MatrixKey: matrixKey, Status: status}
	}

	tests := []struct {
		name            string
		needs           []string
		status          JobStatusMap
		wantOk          bool
		wantTerminal    bool
		wantDetailMatch string // substring; empty = don't check
	}{
		{
			name:   "empty needs is trivially satisfied",
			needs:  nil,
			status: JobStatusMap{},
			wantOk: true,
		},
		{
			name:  "single dep succeeded",
			needs: []string{"build"},
			status: JobStatusMap{
				"build": {row("", "success")},
			},
			wantOk: true,
		},
		{
			name:  "single dep running blocks (non-terminal)",
			needs: []string{"build"},
			status: JobStatusMap{
				"build": {row("", "running")},
			},
			wantOk:          false,
			wantTerminal:    false,
			wantDetailMatch: "build: running",
		},
		{
			name:  "single dep queued blocks (non-terminal)",
			needs: []string{"build"},
			status: JobStatusMap{
				"build": {row("", "queued")},
			},
			wantOk:          false,
			wantTerminal:    false,
			wantDetailMatch: "build: queued",
		},
		{
			name:  "single dep awaiting_approval blocks (non-terminal)",
			needs: []string{"gate"},
			status: JobStatusMap{
				"gate": {row("", "awaiting_approval")},
			},
			wantOk:          false,
			wantTerminal:    false,
			wantDetailMatch: "gate: awaiting_approval",
		},
		{
			name:  "single dep failed cascades (terminal)",
			needs: []string{"build"},
			status: JobStatusMap{
				"build": {row("", "failed")},
			},
			wantOk:          false,
			wantTerminal:    true,
			wantDetailMatch: "build: failed",
		},
		{
			name:  "single dep canceled cascades (terminal)",
			needs: []string{"build"},
			status: JobStatusMap{
				"build": {row("", "canceled")},
			},
			wantOk:          false,
			wantTerminal:    true,
			wantDetailMatch: "build: canceled",
		},
		{
			name:  "single dep skipped cascades (terminal)",
			needs: []string{"build"},
			status: JobStatusMap{
				"build": {row("", "skipped")},
			},
			wantOk:          false,
			wantTerminal:    true,
			wantDetailMatch: "build: skipped",
		},
		{
			name:            "missing dep treated as terminal",
			needs:           []string{"ghost"},
			status:          JobStatusMap{},
			wantOk:          false,
			wantTerminal:    true,
			wantDetailMatch: "ghost: not in this run",
		},
		{
			name:  "empty status slice (defensive) treated as terminal",
			needs: []string{"build"},
			status: JobStatusMap{
				"build": {},
			},
			wantOk:          false,
			wantTerminal:    true,
			wantDetailMatch: "build: no job_run rows",
		},
		{
			name:  "multi dep all succeeded",
			needs: []string{"lint", "typecheck", "unit"},
			status: JobStatusMap{
				"lint":      {row("", "success")},
				"typecheck": {row("", "success")},
				"unit":      {row("", "success")},
			},
			wantOk: true,
		},
		{
			name:  "multi dep first running blocks at first",
			needs: []string{"lint", "typecheck", "unit"},
			status: JobStatusMap{
				"lint":      {row("", "success")},
				"typecheck": {row("", "running")},
				"unit":      {row("", "success")},
			},
			wantOk:          false,
			wantTerminal:    false,
			wantDetailMatch: "typecheck: running",
		},
		{
			name:  "multi dep terminal failure wins over running",
			needs: []string{"lint", "typecheck"},
			status: JobStatusMap{
				"lint":      {row("", "failed")},
				"typecheck": {row("", "running")},
			},
			wantOk:          false,
			wantTerminal:    true, // failed wins via iteration order
			wantDetailMatch: "lint: failed",
		},
		{
			name:  "matrix dep all matrix children succeeded",
			needs: []string{"test"},
			status: JobStatusMap{
				"test": {
					row("node-18", "success"),
					row("node-20", "success"),
					row("node-22", "success"),
				},
			},
			wantOk: true,
		},
		{
			name:  "matrix dep one child still running",
			needs: []string{"test"},
			status: JobStatusMap{
				"test": {
					row("node-18", "success"),
					row("node-20", "running"),
					row("node-22", "success"),
				},
			},
			wantOk:          false,
			wantTerminal:    false,
			wantDetailMatch: "test[node-20]: running",
		},
		{
			name:  "matrix dep one child failed (terminal cascade)",
			needs: []string{"test"},
			status: JobStatusMap{
				"test": {
					row("node-18", "success"),
					row("node-20", "failed"),
					row("node-22", "success"),
				},
			},
			wantOk:          false,
			wantTerminal:    true,
			wantDetailMatch: "test[node-20]: failed",
		},
		{
			name:  "user-reported scenario: build needs 4 jobs, one still running",
			needs: []string{"eslint", "typecheck", "unit", "types-generate"},
			status: JobStatusMap{
				"eslint":         {row("", "success")},
				"typecheck":      {row("", "success")},
				"unit":           {row("", "success")},
				"types-generate": {row("", "running")},
			},
			wantOk:          false,
			wantTerminal:    false,
			wantDetailMatch: "types-generate: running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := needsSatisfied(tt.needs, tt.status)
			if got.Ok != tt.wantOk {
				t.Errorf("Ok = %v, want %v (detail=%q)", got.Ok, tt.wantOk, got.Detail)
			}
			if !tt.wantOk && got.UpstreamTerminal != tt.wantTerminal {
				t.Errorf("UpstreamTerminal = %v, want %v (detail=%q)",
					got.UpstreamTerminal, tt.wantTerminal, got.Detail)
			}
			if tt.wantDetailMatch != "" && !strings.Contains(got.Detail, tt.wantDetailMatch) {
				t.Errorf("Detail = %q, want substring %q", got.Detail, tt.wantDetailMatch)
			}
		})
	}
}

func TestSummarizeNeeds(t *testing.T) {
	t.Parallel()

	row := func(matrixKey, status string) JobStatusRow {
		return JobStatusRow{MatrixKey: matrixKey, Status: status}
	}

	tests := []struct {
		name   string
		needs  []string
		status JobStatusMap
		want   string
	}{
		{
			name:  "all succeeded → empty",
			needs: []string{"a", "b"},
			status: JobStatusMap{
				"a": {row("", "success")},
				"b": {row("", "success")},
			},
			want: "",
		},
		{
			name:  "mix: skip succeeded, list blockers",
			needs: []string{"a", "b", "c"},
			status: JobStatusMap{
				"a": {row("", "success")},
				"b": {row("", "running")},
				"c": {row("", "queued")},
			},
			want: "b: running, c: queued",
		},
		{
			name:  "matrix blocker shows matrix_key",
			needs: []string{"test"},
			status: JobStatusMap{
				"test": {
					row("node-18", "success"),
					row("node-20", "running"),
				},
			},
			want: "test[node-20]: running",
		},
		{
			name:   "missing dep flagged explicitly",
			needs:  []string{"ghost"},
			status: JobStatusMap{},
			want:   "ghost:missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := summarizeNeeds(tt.needs, tt.status); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNeedsSatisfied_ClampsMissingAndNoRowsDetail covers the LOW/SEC
// path the steady-state clamp doesn't reach: the missing-dep and
// no-rows branches in needsSatisfied build the Detail string
// directly (not through describeBlocker), so they need their own
// clamp. The integration test for ghost upstream exercises the
// missing path; this unit test pins the clamp behaviour
// independently of any DB.
func TestNeedsSatisfied_ClampsMissingAndNoRowsDetail(t *testing.T) {
	t.Parallel()

	hugeName := strings.Repeat("a", 1024)

	t.Run("missing dep", func(t *testing.T) {
		t.Parallel()
		got := needsSatisfied([]string{hugeName}, JobStatusMap{})
		if got.Ok {
			t.Fatalf("expected not satisfied for missing dep")
		}
		// "name: not in this run" — name clamped to 128 + 18 chars
		// suffix = 146 chars max.
		if len(got.Detail) > 160 {
			t.Errorf("Detail len = %d, want clamped (~146); leak from missing-dep path",
				len(got.Detail))
		}
	})

	t.Run("no_rows defensive", func(t *testing.T) {
		t.Parallel()
		got := needsSatisfied([]string{hugeName}, JobStatusMap{hugeName: nil})
		if got.Ok {
			t.Fatalf("expected not satisfied for empty rows")
		}
		// "name: no job_run rows" — clamped to ~150 max.
		if len(got.Detail) > 160 {
			t.Errorf("Detail len = %d, want clamped; leak from no-rows path",
				len(got.Detail))
		}
	})
}

// TestSummarizeNeeds_ClampsMissingNameInSummary verifies the
// missing-dep formatting in summarizeNeeds clamps the name. Without
// this, a pipeline with one absurdly-named ghost dep could blow up
// the structured-log "blockers" field per dispatch tick.
func TestSummarizeNeeds_ClampsMissingNameInSummary(t *testing.T) {
	t.Parallel()

	hugeName := strings.Repeat("z", 1024)
	got := summarizeNeeds([]string{hugeName}, JobStatusMap{})
	// One blocker "name:missing" — name clamped to 128, plus 8
	// chars (":missing") = 136 chars max.
	if len(got) > 150 {
		t.Errorf("summary len = %d, want clamped; leak from missing-name path",
			len(got))
	}
}

// TestDescribeBlocker_ClampsRunawayName is the LOW/SEC guard: the
// parser doesn't bound job names today, so a pathologically long
// YAML name would otherwise flow verbatim into the job_runs.error
// column AND every structured log line. Mirrors the
// clampBytes pattern from the grpcsrv cleanup-ack handler.
func TestDescribeBlocker_ClampsRunawayName(t *testing.T) {
	t.Parallel()

	hugeName := strings.Repeat("a", 1024)
	hugeMatrix := strings.Repeat("m", 1024)
	hugeStatus := strings.Repeat("s", 1024)

	got := describeBlocker(hugeName, JobStatusRow{MatrixKey: hugeMatrix, Status: hugeStatus})

	// Format is "name[matrix]: status" — three clamped fields plus
	// 4 fixed bytes ("[", "]", ":", " "). At most 3*128 + 4 = 388.
	if len(got) > 388 {
		t.Errorf("describeBlocker output len = %d, want ≤388 (runaway fields not clamped)", len(got))
	}
	// Sanity: the first 128 'a's of the name must be present (clamp
	// shouldn't drop the start of the string).
	if !strings.HasPrefix(got, strings.Repeat("a", 128)) {
		t.Errorf("describeBlocker output didn't preserve the clamped prefix")
	}
}

// Regression: the summary line must not blow up the structured log
// entry. 100 blocked deps with long names would otherwise produce a
// multi-KB log line per tick.
func TestSummarizeNeeds_TrimsRunaway(t *testing.T) {
	t.Parallel()

	needs := make([]string, 100)
	status := JobStatusMap{}
	for i := range needs {
		// Each name is ~30 chars; 100 of them = ~3KB before trim.
		needs[i] = strings.Repeat("a", 30)
		status[needs[i]] = []JobStatusRow{{Status: "running"}}
	}
	got := summarizeNeeds(needs, status)
	// 240 bytes prefix + 3-byte UTF-8 "…" = 243 max.
	if len(got) > 243 {
		t.Errorf("summary len = %d, want ≤243; runaway not trimmed", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("summary should end with ellipsis when trimmed, got %q", got[max(0, len(got)-10):])
	}
}
