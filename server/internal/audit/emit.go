// Package audit wraps store.EmitAuditEvent with the "pull actor
// from request context" boilerplate every HTTP handler needs.
// Handlers import this instead of touching store directly so the
// "how do we identify the actor?" decision lives in one place;
// a future change (e.g. API tokens with their own actor type)
// only has to update this package.
package audit

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Emit writes a row. Non-blocking semantic: if the DB insert
// fails, the action the operator requested already succeeded —
// we log the audit failure and move on rather than rolling back
// a successful deploy because a row in audit_events couldn't
// land. Callers normally fire-and-forget.
//
// Actor is pulled from the request context. Anonymous requests
// (auth disabled, webhook-driven code paths) record a system
// event with nil actor_id + empty actor_email.
func Emit(
	ctx context.Context,
	log *slog.Logger,
	s *store.Store,
	action, targetType, targetID string,
	metadata map[string]any,
) {
	var actorID uuid.UUID
	var actorEmail string
	if u, ok := authapi.UserFromContext(ctx); ok {
		actorID = u.ID
		actorEmail = u.Email
	}
	if _, err := s.EmitAuditEvent(ctx, store.AuditEmit{
		ActorID:    actorID,
		ActorEmail: actorEmail,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Metadata:   metadata,
	}); err != nil {
		if log != nil {
			log.Warn("audit emit failed",
				"action", action,
				"target_type", targetType,
				"target_id", targetID,
				"err", err)
		}
	}
}
