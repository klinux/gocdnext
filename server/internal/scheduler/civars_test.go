package scheduler

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestBuildCIVars(t *testing.T) {
	runID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	pipelineID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	projectID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	materialA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	materialZ := "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"
	const fullSHA = "f5b5f8a66a753e4fc64fc80ec518ad27be57e75c"

	tests := []struct {
		name    string
		run     store.RunForDispatch
		jobName string
		want    map[string]string
	}{
		{
			name: "single-material run carries commit + branch + counter",
			run: store.RunForDispatch{
				ID:         runID,
				PipelineID: pipelineID,
				ProjectID:  projectID,
				Counter:    42,
				Revisions:  json.RawMessage(`{"` + materialA + `":{"revision":"` + fullSHA + `","branch":"gocdnext-tests"}}`),
			},
			jobName: "buildx",
			want: map[string]string{
				"CI":                  "true",
				"GOCDNEXT":            "true",
				"CI_RUN_ID":           runID.String(),
				"CI_RUN_COUNTER":      "42",
				"CI_PIPELINE_ID":      pipelineID.String(),
				"CI_PROJECT_ID":       projectID.String(),
				"CI_JOB_NAME":         "buildx",
				"CI_COMMIT_SHA":       fullSHA,
				"CI_COMMIT_SHORT_SHA": fullSHA[:8],
				"CI_BRANCH":           "gocdnext-tests",
			},
		},
		{
			name: "multi-material run picks lowest-uuid material deterministically",
			run: store.RunForDispatch{
				ID:         runID,
				PipelineID: pipelineID,
				ProjectID:  projectID,
				Counter:    1,
				// `aaa…` sorts before `zzz…` so the A entry must win
				// regardless of map iteration order.
				Revisions: json.RawMessage(`{
					"` + materialZ + `":{"revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","branch":"feature"},
					"` + materialA + `":{"revision":"` + fullSHA + `","branch":"gocdnext-tests"}
				}`),
			},
			jobName: "compile",
			want: map[string]string{
				"CI":                  "true",
				"GOCDNEXT":            "true",
				"CI_RUN_ID":           runID.String(),
				"CI_RUN_COUNTER":      "1",
				"CI_PIPELINE_ID":      pipelineID.String(),
				"CI_PROJECT_ID":       projectID.String(),
				"CI_JOB_NAME":         "compile",
				"CI_COMMIT_SHA":       fullSHA,
				"CI_COMMIT_SHORT_SHA": fullSHA[:8],
				"CI_BRANCH":           "gocdnext-tests",
			},
		},
		{
			name: "manual trigger without revisions leaves commit/branch unset",
			run: store.RunForDispatch{
				ID:         runID,
				PipelineID: pipelineID,
				ProjectID:  projectID,
				Counter:    7,
				Revisions:  json.RawMessage(`{}`),
			},
			jobName: "deploy",
			want: map[string]string{
				// No CI_COMMIT_* / CI_BRANCH keys when there's no
				// revision — the substitution layer then leaves
				// `${CI_COMMIT_SHORT_SHA}` literal so the run fails
				// fast at dispatch with a useful error instead of
				// publishing an image tagged `myapp:1.7.`.
				"CI":             "true",
				"GOCDNEXT":       "true",
				"CI_RUN_ID":      runID.String(),
				"CI_RUN_COUNTER": "7",
				"CI_PIPELINE_ID": pipelineID.String(),
				"CI_PROJECT_ID":  projectID.String(),
				"CI_JOB_NAME":    "deploy",
			},
		},
		{
			name: "short commit truncates at 8 chars; sub-8 commit kept verbatim",
			run: store.RunForDispatch{
				ID: runID, PipelineID: pipelineID, ProjectID: projectID, Counter: 1,
				Revisions: json.RawMessage(`{"` + materialA + `":{"revision":"abc1234","branch":"x"}}`),
			},
			jobName: "j",
			want: map[string]string{
				"CI": "true", "GOCDNEXT": "true",
				"CI_RUN_ID":           runID.String(),
				"CI_RUN_COUNTER":      "1",
				"CI_PIPELINE_ID":      pipelineID.String(),
				"CI_PROJECT_ID":       projectID.String(),
				"CI_JOB_NAME":         "j",
				"CI_COMMIT_SHA":       "abc1234",
				"CI_COMMIT_SHORT_SHA": "abc1234", // 7 chars stays
				"CI_BRANCH":           "x",
			},
		},
		{
			name: "malformed revisions JSON degrades cleanly to no commit",
			run: store.RunForDispatch{
				ID: runID, PipelineID: pipelineID, ProjectID: projectID, Counter: 1,
				Revisions: json.RawMessage(`{garbage`),
			},
			jobName: "j",
			want: map[string]string{
				"CI": "true", "GOCDNEXT": "true",
				"CI_RUN_ID":      runID.String(),
				"CI_RUN_COUNTER": "1",
				"CI_PIPELINE_ID": pipelineID.String(),
				"CI_PROJECT_ID":  projectID.String(),
				"CI_JOB_NAME":    "j",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCIVars(tc.run, tc.jobName)
			if len(got) != len(tc.want) {
				t.Errorf("keys = %v, want %v", keys(got), keys(tc.want))
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("CI vars[%q] = %q, want %q", k, got[k], v)
				}
			}
			for k := range got {
				if _, ok := tc.want[k]; !ok {
					t.Errorf("unexpected key %q = %q", k, got[k])
				}
			}
		})
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
