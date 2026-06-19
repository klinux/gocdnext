package secrets

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// backendReloadBackoff paces LISTEN re-dials after a dropped connection.
// Backend config changes are rare admin actions, so a few seconds of listener
// downtime only widens the cross-replica window back toward the registry TTL
// — never past it.
const backendReloadBackoff = 5 * time.Second

// backendNoticeHandler is the listener's view of the registry.
type backendNoticeHandler interface {
	HandleNotice(payload string)
}

// ListenForBackendChanges blocks until ctx cancels, holding a dedicated
// Postgres connection LISTENing on store.SecretBackendsChannel and forwarding
// each notification (the changed source) to the registry's HandleNotice. This
// is the cross-replica half of hot-reload: the replica handling the admin
// write fires the NOTIFY inside its tx (commit-gated); every other replica
// hears it and converges within milliseconds instead of waiting out the
// registry TTL. Connection loss re-dials with backoff; the TTL stays the
// correctness backstop.
func ListenForBackendChanges(ctx context.Context, dsn string, h backendNoticeHandler, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	for ctx.Err() == nil {
		if err := listenBackendOnce(ctx, dsn, h, log); err != nil && ctx.Err() == nil {
			log.Warn("secret backend listener: reconnecting", "err", err, "backoff", backendReloadBackoff)
			select {
			case <-time.After(backendReloadBackoff):
			case <-ctx.Done():
			}
		}
	}
}

func listenBackendOnce(ctx context.Context, dsn string, h backendNoticeHandler, log *slog.Logger) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, "LISTEN "+store.SecretBackendsChannel); err != nil {
		return err
	}
	log.Info("secret backend listener started", "channel", store.SecretBackendsChannel)
	for {
		note, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		log.Info("secret backend change notice", "source", note.Payload)
		h.HandleNotice(note.Payload)
	}
}
