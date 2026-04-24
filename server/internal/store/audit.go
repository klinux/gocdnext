package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// AuditActions are the canonical action names the audit log
// carries. Centralised so the admin UI + filter query share a
// vocabulary and a typo at a call site surfaces as a
// compile-time miss instead of a silent orphan in the log.
const (
	AuditActionProjectApply        = "project.apply"
	AuditActionProjectDelete       = "project.delete"
	AuditActionProjectSync         = "project.sync"
	AuditActionSecretSet           = "secret.set"
	AuditActionSecretDelete        = "secret.delete"
	AuditActionGlobalSecretSet     = "global_secret.set"
	AuditActionGlobalSecretDelete  = "global_secret.delete"
	AuditActionCachePurge          = "cache.purge"
	AuditActionRunTrigger          = "run.trigger"
	AuditActionRunCancel           = "run.cancel"
	AuditActionRunRerun            = "run.rerun"
	AuditActionJobRerun            = "job.rerun"
	AuditActionApprovalApprove     = "approval.approve"
	AuditActionApprovalReject      = "approval.reject"
	AuditActionUserRoleChange      = "user.role_change"
	AuditActionWebhookSecretRotate = "webhook_secret.rotate"
	AuditActionProjectNotifsSet    = "project_notifications.set"
)

// AuditEvent is what the audit log surfaces to callers. Metadata
// is kept as raw JSON so handlers can stamp arbitrary structured
// shapes without a per-action schema — the admin UI treats it as
// displayable key/value.
type AuditEvent struct {
	ID         uuid.UUID       `json:"id"`
	ActorID    *uuid.UUID      `json:"actor_id,omitempty"`
	ActorEmail string          `json:"actor_email,omitempty"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id,omitempty"`
	Metadata   json.RawMessage `json:"metadata"`
	At         time.Time       `json:"at"`
}

// AuditEmit is the per-call input the handler builds. Actor +
// email come from the authenticated session when present; leaving
// both zero records a system event (webhook auto-created run,
// cron trigger, etc.).
type AuditEmit struct {
	ActorID    uuid.UUID // uuid.Nil = system
	ActorEmail string
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any // marshalled to JSONB; nil = {}
}

// EmitAuditEvent writes a row. Best-effort logging: audit
// failures must NOT break the caller — if the log table is
// unreachable the action still succeeds and the failure is
// reported in the server log. Return the error so telemetry can
// pick it up, but callers typically `_ =` the result.
func (s *Store) EmitAuditEvent(ctx context.Context, in AuditEmit) (AuditEvent, error) {
	if in.Action == "" || in.TargetType == "" {
		return AuditEvent{}, fmt.Errorf("store: audit emit: action and target_type required")
	}
	var metaBytes []byte
	if len(in.Metadata) == 0 {
		metaBytes = []byte(`{}`)
	} else {
		b, err := json.Marshal(in.Metadata)
		if err != nil {
			return AuditEvent{}, fmt.Errorf("store: audit emit: marshal metadata: %w", err)
		}
		metaBytes = b
	}
	row, err := s.q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ActorID:     nullableUUID(in.ActorID),
		ActorEmail:  in.ActorEmail,
		Action:      in.Action,
		TargetType:  in.TargetType,
		TargetID:    in.TargetID,
		Metadata:    metaBytes,
	})
	if err != nil {
		return AuditEvent{}, fmt.Errorf("store: audit emit: %w", err)
	}
	return auditRowToEvent(row), nil
}

// ListAuditEventsFilter captures every filter the admin UI
// surfaces. Zero value on any field disables that filter —
// empty string skips string filters, uuid.Nil skips actor.
type ListAuditEventsFilter struct {
	Action      string
	TargetType  string
	ActorEmail  string
	ActorID     uuid.UUID
	Limit       int32
}

// ListAuditEvents returns the most recent events matching the
// filter, newest first. Limit caps the page size — the
// admin UI passes 100; pagination cursor lands in a later
// iteration if operators need to scroll deeper.
func (s *Store) ListAuditEvents(ctx context.Context, f ListAuditEventsFilter) ([]AuditEvent, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	rows, err := s.q.ListAuditEvents(ctx, db.ListAuditEventsParams{
		Column1: f.Action,
		Column2: f.TargetType,
		Column3: f.ActorEmail,
		Column4: nullableUUID(f.ActorID),
		Limit:   f.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	out := make([]AuditEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, auditRowToEvent(r))
	}
	return out, nil
}

// nullableUUID returns a pgtype.UUID where uuid.Nil maps to the
// SQL NULL representation. Keeping the translation here (vs. at
// every call site) ensures the audit table's "no actor"
// semantics stay the single definition of system-driven events.
func nullableUUID(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{Valid: false}
	}
	return pgUUID(id)
}

func auditRowToEvent(r db.AuditEvent) AuditEvent {
	ev := AuditEvent{
		ID:         fromPgUUID(r.ID),
		ActorEmail: r.ActorEmail,
		Action:     r.Action,
		TargetType: r.TargetType,
		TargetID:   r.TargetID,
		Metadata:   json.RawMessage(r.Metadata),
		At:         r.At.Time,
	}
	if r.ActorID.Valid {
		id := fromPgUUID(r.ActorID)
		ev.ActorID = &id
	}
	return ev
}
