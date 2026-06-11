package oidcissuer

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// reconnectBackoff paces LISTEN re-dials after a dropped
// connection. Rotations are rare admin actions, so a few seconds
// of listener downtime only widens the cross-replica window back
// toward the keyCacheTTL bound — never past it.
const reconnectBackoff = 5 * time.Second

// RotationNoticeHandler is the listener's view of the issuer. The
// kid payload lets the handler skip the invalidation when its cache
// already holds the new key (the rotating replica hears its own
// NOTIFY — dumping the freshly-primed cache would force a useless
// refetch right after every rotation).
type RotationNoticeHandler interface {
	HandleRotationNotice(kid string)
}

// ListenForRotations blocks until ctx cancels, holding a dedicated
// Postgres connection LISTENing on store.OIDCKeysChannel and
// forwarding each notification to the issuer's
// HandleRotationNotice (idempotent by kid — a notice for the key
// already cached preserves it and only refreshes the JWKS
// document). This is the cross-replica half of key rotation: the
// replica that handles the admin request swaps atomically under
// its own lock; every other replica hears the NOTIFY (fired inside
// the rotation tx, so commit-gated) and converges typically within
// milliseconds, instead of waiting out keyCacheTTL.
//
// Connection loss re-dials with backoff. The cache TTL remains the
// correctness backstop — the listener is a latency optimisation
// that shrinks remote convergence from "within 60s" to ~ms.
func ListenForRotations(ctx context.Context, dsn string, inv RotationNoticeHandler, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	for ctx.Err() == nil {
		if err := listenOnce(ctx, dsn, inv, log); err != nil && ctx.Err() == nil {
			log.Warn("oidc rotation listener: reconnecting", "err", err, "backoff", reconnectBackoff)
			select {
			case <-time.After(reconnectBackoff):
			case <-ctx.Done():
			}
		}
	}
}

func listenOnce(ctx context.Context, dsn string, inv RotationNoticeHandler, log *slog.Logger) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, "LISTEN "+store.OIDCKeysChannel); err != nil {
		return err
	}
	log.Info("oidc rotation listener started", "channel", store.OIDCKeysChannel)
	for {
		note, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		log.Info("oidc key rotation notice", "kid", note.Payload)
		inv.HandleRotationNotice(note.Payload)
	}
}
