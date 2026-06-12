package webhook

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// skipCIMarkers are the platform-agnostic commit-message markers that
// suppress run creation, in priority order for logging. Deliberately
// only the cross-CI conventions ([skip ci]/[ci skip] are honored by
// GitHub Actions, GitLab CI and Woodpecker; [no ci] by GitHub Actions
// and CircleCI) — product-specific spellings like [skip actions] stay
// out so the contract is portable both ways.
//
// Scope (enforced at the call sites, not here): branch pushes and tag
// pushes only. pull_request events are NEVER skip-checked — honoring
// the marker there would let any contributor bypass PR validation by
// writing it into their own commit, turning a convenience into a
// security hole.
var skipCIMarkers = []string{"[skip ci]", "[ci skip]", "[no ci]"}

// skipCIMarker reports whether the commit message asks CI to be
// skipped, returning the (canonical, lowercase) marker that matched
// for logging. Case-insensitive, matches anywhere in the message —
// title or body — same semantics as GitHub Actions.
func skipCIMarker(message string) (string, bool) {
	if message == "" {
		return "", false
	}
	lower := strings.ToLower(message)
	for _, m := range skipCIMarkers {
		if strings.Contains(lower, m) {
			return m, true
		}
	}
	return "", false
}

// respondSkipCI acknowledges a delivery whose commit message carries a
// skip marker: distinct delivery status (operators can filter "skipped
// by marker" apart from "didn't match anything") and a 200 with the
// matched marker in the body, so the provider's redelivery view shows
// WHY nothing ran without log access.
func (h *Handler) respondSkipCI(w http.ResponseWriter, rec *deliveryRec, provider, delivery, ref, marker string) {
	rec.status = store.WebhookStatusSkipped
	h.log.Info(provider+" webhook: run creation skipped by commit marker",
		"delivery", delivery, "ref", ref, "marker", marker)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"skipped_by": marker})
}
