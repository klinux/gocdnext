package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// oidcKeyDTO is the lifecycle view of one signing key. Key material
// (even the public DER) deliberately never appears here — the JWKS
// endpoint is the public-key surface; the admin UI shows lifecycle.
type oidcKeyDTO struct {
	ID        string     `json:"id"`
	Kid       string     `json:"kid"`
	Alg       string     `json:"alg"`
	CreatedAt time.Time  `json:"created_at"`
	RetiredAt *time.Time `json:"retired_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// OIDCKeys handles GET /api/v1/admin/oidc/keys — every key the
// table holds (active + retired + revoked), newest first, so the
// admin can audit the rotation history.
func (h *Handler) OIDCKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.ListOIDCKeysAdmin(r.Context())
	if err != nil {
		h.log.Error("admin: list oidc keys", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	keys := make([]oidcKeyDTO, 0, len(rows))
	for _, k := range rows {
		keys = append(keys, oidcKeyDTO{
			ID:        k.ID.String(),
			Kid:       k.Kid,
			Alg:       k.Alg,
			CreatedAt: k.CreatedAt,
			RetiredAt: k.RetiredAt,
			RevokedAt: k.RevokedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

type rotateOIDCKeyRequest struct {
	// Mode: "graceful" (default — old key keeps verifying in the
	// JWKS until in-flight tokens expire) or "emergency" (key
	// compromise: old key leaves the JWKS immediately, outstanding
	// tokens become unverifiable).
	Mode string `json:"mode"`
}

// RotateOIDCKey handles POST /api/v1/admin/oidc/keys/rotate. Wired
// under the admin-only route group; audit-logged with the mode and
// the old/new kid pair so a compromise response is reconstructable.
func (h *Handler) RotateOIDCKey(w http.ResponseWriter, r *http.Request) {
	var req rotateOIDCKeyRequest
	if r.Body != nil {
		// Empty body = graceful default. A malformed body is a 400 —
		// silently defaulting on garbage could turn an intended
		// emergency rotation into a graceful one.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil && err.Error() != "EOF" {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	emergency := false
	switch req.Mode {
	case "", "graceful":
		req.Mode = "graceful"
	case "emergency":
		emergency = true
	default:
		http.Error(w, `mode must be "graceful" or "emergency"`, http.StatusBadRequest)
		return
	}

	// Route through the issuer when it's enabled: Rotate holds the
	// issuer's signing-key mutex across the DB commit AND the cache
	// swap, so no concurrent dispatch can sign with a just-revoked
	// key in between (review round 2). Other replicas converge via
	// the Postgres NOTIFY fired inside the rotation tx (see
	// oidcissuer/listen.go). Issuer disabled → store directly;
	// there are no caches to race.
	var fresh store.OIDCSigningKey
	var err error
	if h.oidcRotator != nil {
		fresh, err = h.oidcRotator.Rotate(r.Context(), emergency)
	} else {
		fresh, err = h.store.RotateOIDCKey(r.Context(), emergency)
	}
	if err != nil {
		h.log.Error("admin: rotate oidc key", "mode", req.Mode, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionOIDCKeyRotate, "oidc_key", fresh.Kid,
		map[string]any{"mode": req.Mode})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kid":  fresh.Kid,
		"mode": req.Mode,
		// Operators verifying a graceful rotation want to know when
		// the old key is safe to assume gone from verifier caches.
		"note": "retired keys stay in the JWKS until in-flight tokens expire; emergency-revoked keys are removed immediately (verifiers may cache the JWKS up to 5 minutes)",
	})
}
