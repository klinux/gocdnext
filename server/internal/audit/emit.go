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

// EmitAs records an event for a NON-HUMAN actor — today, a pipeline reconciling the
// deploy target it declares in its own repo. Emit pulls the actor from the request
// context, but a dispatch-time reconcile has no request and no user, so the row would
// otherwise land with a nil actor and an empty email: indistinguishable from every other
// system event, right where "who changed this deploy target?" has to be answerable.
//
// actorLabel is a RESERVED, non-email string (e.g. "pipeline:shop/release") stored in
// actor_email. No schema change is needed: actor_email is TEXT NOT NULL DEFAULT ” with
// no CHECK, actor_id is nullable, and uuid.Nil already lands as SQL NULL through
// nullableUUID — the column's own migration comment anticipates a "system" sentinel.
func EmitAs(
	ctx context.Context,
	log *slog.Logger,
	s *store.Store,
	actorLabel string,
	action, targetType, targetID string,
	metadata map[string]any,
) {
	if _, err := s.EmitAuditEvent(ctx, store.AuditEmit{
		ActorID:    uuid.Nil,
		ActorEmail: actorLabel,
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
				"actor", actorLabel,
				"err", err)
		}
	}
}
