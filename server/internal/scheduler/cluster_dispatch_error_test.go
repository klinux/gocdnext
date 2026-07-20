package scheduler_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// The dispatch-time `cluster:` error must collapse the missing-vs-unauthorized
// oracle in the run-facing message, while a non-oracle failure keeps its detail.
func TestClusterDispatchError(t *testing.T) {
	t.Run("oracle sentinels collapse to the generic message", func(t *testing.T) {
		for _, err := range []error{
			store.ErrClusterNotFound,
			fmt.Errorf(`resolve %q: %w`, "secret-prod", store.ErrClusterNotAuthorized),
		} {
			msg, collapsed := scheduler.ClusterDispatchError(err)
			if !collapsed {
				t.Errorf("ClusterDispatchError(%v) collapsed=false, want true", err)
			}
			if msg != "cluster: "+store.ClusterUnavailableMessage {
				t.Errorf("msg = %q, want the generic collapsed message", msg)
			}
			if strings.Contains(msg, "not authorized") || strings.Contains(msg, "store:") ||
				strings.Contains(msg, "secret-prod") {
				t.Errorf("run-facing message leaked the internal reason: %q", msg)
			}
		}
	})

	t.Run("non-oracle failure keeps its detail for the operator", func(t *testing.T) {
		msg, collapsed := scheduler.ClusterDispatchError(errors.New("dial tcp 10.0.0.5:6443: i/o timeout"))
		if collapsed {
			t.Errorf("a transport error must not be collapsed")
		}
		if !strings.Contains(msg, "i/o timeout") {
			t.Errorf("msg = %q, want it to keep the transport detail", msg)
		}
	})
}
