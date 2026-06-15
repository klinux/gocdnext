package webhook

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func TestFilterMaterialsByEvent(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mk := func(events []string) store.Material {
		cfg, err := json.Marshal(domain.GitMaterial{Events: events})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return store.Material{Config: cfg}
	}
	// Same fingerprint, different when.event lowered onto each material.
	materials := []store.Material{
		mk([]string{"push"}),                 // 0 push-only
		mk([]string{"pull_request", "push"}), // 1 PR + push
		mk([]string{"tag"}),                  // 2 tag-only
		mk([]string{"manual"}),               // 3 manual-only
		mk(nil),                              // 4 no on: → defaults to push
	}

	cases := []struct {
		event        string
		wantKept     int
		wantFiltered int
	}{
		// push keeps push-only, PR+push, and the empty-default; drops
		// tag-only + manual-only (the bug those two slipped through).
		{"push", 3, 2},
		{"tag", 1, 4},          // only the tag-only material
		{"pull_request", 1, 4}, // only the PR+push material
	}
	for _, c := range cases {
		t.Run(c.event, func(t *testing.T) {
			kept, filtered := filterMaterialsByEvent(log, materials, c.event, "github", "d")
			if len(kept) != c.wantKept || filtered != c.wantFiltered {
				t.Fatalf("event %s: kept=%d filtered=%d, want %d/%d",
					c.event, len(kept), filtered, c.wantKept, c.wantFiltered)
			}
		})
	}

	t.Run("manual-only never fires from any real webhook event", func(t *testing.T) {
		for _, ev := range []string{"push", "tag", "pull_request"} {
			kept, _ := filterMaterialsByEvent(log, []store.Material{mk([]string{"manual"})}, ev, "github", "d")
			if len(kept) != 0 {
				t.Fatalf("manual-only material survived %s event", ev)
			}
		}
	})

	t.Run("undecodable config kept (fail-open)", func(t *testing.T) {
		bad := store.Material{Config: json.RawMessage(`{not valid`)}
		kept, filtered := filterMaterialsByEvent(log, []store.Material{bad}, "push", "github", "d")
		if len(kept) != 1 || filtered != 0 {
			t.Fatalf("undecodable config should be kept (fan-out owns the failure): kept=%d filtered=%d", len(kept), filtered)
		}
	})
}
