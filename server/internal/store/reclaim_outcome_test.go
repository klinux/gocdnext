package store_test

import (
	"errors"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// OutcomeLabel is the bounded Prometheus label mapping for
// gocdnext_jobs_reclaimed_total{outcome}. Err must win because the fail-at-max
// error branch leaves Action at its "" zero value; an unset Action with no error
// falls through to "skipped".
func TestReclaimResult_OutcomeLabel(t *testing.T) {
	tests := []struct {
		name string
		res  store.ReclaimResult
		want string
	}{
		{"error wins over action", store.ReclaimResult{Err: errors.New("db"), Action: store.ReclaimActionRequeued}, "error"},
		{"requeued", store.ReclaimResult{Action: store.ReclaimActionRequeued}, "requeued"},
		{"failed maps to failed_max", store.ReclaimResult{Action: store.ReclaimActionFailed}, "failed_max"},
		{"skipped", store.ReclaimResult{Action: store.ReclaimActionSkipped}, "skipped"},
		{"zero action defaults to skipped", store.ReclaimResult{}, "skipped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.res.OutcomeLabel(); got != tt.want {
				t.Fatalf("OutcomeLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
