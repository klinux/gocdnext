package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// IsClusterUnavailable must collapse BOTH the missing and the not-authorized
// sentinel (and their wrapped forms) so neither flow can leak a cross-project
// cluster existence oracle — but must NOT swallow unrelated errors.
func TestIsClusterUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"not found", store.ErrClusterNotFound, true},
		{"not authorized", store.ErrClusterNotAuthorized, true},
		{"wrapped not found", fmt.Errorf("resolve: %w", store.ErrClusterNotFound), true},
		{"wrapped not authorized", fmt.Errorf(`%w "prod"`, store.ErrClusterNotAuthorized), true},
		{"in use is not unavailable", store.ErrClusterInUse, false},
		{"unrelated", errors.New("dial tcp: i/o timeout"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := store.IsClusterUnavailable(tt.err); got != tt.want {
				t.Errorf("IsClusterUnavailable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
