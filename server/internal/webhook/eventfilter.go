package webhook

import (
	"encoding/json"
	"log/slog"
	"slices"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// filterMaterialsByEvent drops materials whose lowered `events:` (the
// pipeline's `when.event`, carried onto the implicit project material)
// does not include `event`. The fingerprint match (URL+branch) is a
// necessary but NOT sufficient authorization to fire: a pipeline
// declaring `when.event: [tag]` or `[manual]` still gets an implicit
// material on the default branch whose fingerprint a plain push
// matches — yet it must not run on push. The tag-push and pull_request
// paths already apply this guard inline; the branch-push path lacked
// it, so tag/manual pipelines fanned out on any push to the material's
// branch.
//
// Empty events mirrors the parser default (`["push"]`) so a material
// with no `on:` still fires on push — and only push. Returns the
// survivors plus how many were filtered (for the response body / a
// structured log). Undecodable config is kept: the fan-out path
// already owns that failure mode, and dropping silently would hide a
// real run.
func filterMaterialsByEvent(
	log *slog.Logger,
	materials []store.Material,
	event string,
	provider, delivery string,
) (kept []store.Material, filtered int) {
	kept = make([]store.Material, 0, len(materials))
	for _, m := range materials {
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			log.Warn(provider+" webhook: decode material config (event filter)",
				"delivery", delivery, "material_id", m.ID, "err", err)
			kept = append(kept, m)
			continue
		}
		events := cfg.Events
		if len(events) == 0 {
			events = []string{"push"} // mirror parser defaultEvents
		}
		if slices.Contains(events, event) {
			kept = append(kept, m)
			continue
		}
		filtered++
		log.Info(provider+" webhook: material filtered by when.event",
			"delivery", delivery, "material_id", m.ID,
			"event", event, "material_events", cfg.Events)
	}
	return kept, filtered
}
