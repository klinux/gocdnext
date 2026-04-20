package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrWebhookDeliveryNotFound is returned by GetWebhookDelivery when
// the BIGSERIAL id doesn't match any row.
var ErrWebhookDeliveryNotFound = errors.New("store: webhook delivery not found")

// Webhook delivery status strings. Kept as constants so handlers
// and the admin UI agree on the vocabulary.
const (
	WebhookStatusAccepted = "accepted"
	WebhookStatusRejected = "rejected"
	WebhookStatusError    = "error"
	WebhookStatusIgnored  = "ignored"
)

// InsertWebhookDeliveryInput captures everything the admin console
// needs to investigate a delivery after the fact. Headers and
// payload are stored as raw JSONB so we can show them verbatim in
// the drawer.
type InsertWebhookDeliveryInput struct {
	Provider   string
	Event      string
	MaterialID uuid.UUID // uuid.Nil when no material matched (NULL FK)
	Status     string
	HTTPStatus int32
	Headers    json.RawMessage
	Payload    json.RawMessage
	Error      string
}

// InsertWebhookDelivery appends an audit row. Returns the id +
// received_at stamp so handlers can log them. A DB failure here
// should not be fatal for the caller — the handler logs and
// carries on so a stuck audit table doesn't swallow pipelines.
func (s *Store) InsertWebhookDelivery(ctx context.Context, in InsertWebhookDeliveryInput) (int64, time.Time, error) {
	matID := pgtype.UUID{}
	if in.MaterialID != uuid.Nil {
		matID = pgtype.UUID{Bytes: in.MaterialID, Valid: true}
	}
	var errStr *string
	if in.Error != "" {
		v := in.Error
		errStr = &v
	}
	row, err := s.q.InsertWebhookDelivery(ctx, db.InsertWebhookDeliveryParams{
		Provider:   in.Provider,
		Event:      in.Event,
		MaterialID: matID,
		Status:     in.Status,
		HttpStatus: in.HTTPStatus,
		Headers:    rawOrNull(in.Headers),
		Payload:    rawOrNull(in.Payload),
		Error:      errStr,
	})
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("store: insert webhook delivery: %w", err)
	}
	return row.ID, row.ReceivedAt.Time, nil
}

// WebhookDeliverySummary is the list-page row shape.
type WebhookDeliverySummary struct {
	ID         int64      `json:"id"`
	Provider   string     `json:"provider"`
	Event      string     `json:"event"`
	MaterialID *uuid.UUID `json:"material_id,omitempty"`
	Status     string     `json:"status"`
	HTTPStatus int32      `json:"http_status"`
	Error      string     `json:"error,omitempty"`
	ReceivedAt time.Time  `json:"received_at"`
}

// WebhookDeliveryDetail adds headers + payload on top of the
// summary. Drawer-only; the list shot stays skinny.
type WebhookDeliveryDetail struct {
	WebhookDeliverySummary
	Headers json.RawMessage `json:"headers,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// WebhookDeliveryFilter bundles the admin page's filter chips.
type WebhookDeliveryFilter struct {
	Provider string
	Status   string
}

// ListWebhookDeliveries returns a paginated slice filtered by
// optional provider + status. Matches the shape of ListRunsGlobal
// so the admin page and /runs page can share table helpers.
func (s *Store) ListWebhookDeliveries(ctx context.Context, limit int32, offset int64, filter WebhookDeliveryFilter) ([]WebhookDeliverySummary, error) {
	if offset < 0 {
		offset = 0
	}
	rows, err := s.q.ListWebhookDeliveries(ctx, db.ListWebhookDeliveriesParams{
		Limit:          limit,
		ProviderFilter: filter.Provider,
		StatusFilter:   filter.Status,
		RowOffset:      offset,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list webhook deliveries: %w", err)
	}
	out := make([]WebhookDeliverySummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, WebhookDeliverySummary{
			ID:         r.ID,
			Provider:   r.Provider,
			Event:      r.Event,
			MaterialID: uuidPtrOrNil(r.MaterialID),
			Status:     r.Status,
			HTTPStatus: r.HttpStatus,
			Error:      stringValue(r.Error),
			ReceivedAt: r.ReceivedAt.Time,
		})
	}
	return out, nil
}

// CountWebhookDeliveries returns the total matching the filter
// (used for pagination display).
func (s *Store) CountWebhookDeliveries(ctx context.Context, filter WebhookDeliveryFilter) (int64, error) {
	n, err := s.q.CountWebhookDeliveries(ctx, db.CountWebhookDeliveriesParams{
		ProviderFilter: filter.Provider,
		StatusFilter:   filter.Status,
	})
	if err != nil {
		return 0, fmt.Errorf("store: count webhook deliveries: %w", err)
	}
	return n, nil
}

// GetWebhookDelivery returns a single row with full headers +
// payload. Errors map pgx.ErrNoRows to ErrWebhookDeliveryNotFound.
func (s *Store) GetWebhookDelivery(ctx context.Context, id int64) (WebhookDeliveryDetail, error) {
	row, err := s.q.GetWebhookDelivery(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return WebhookDeliveryDetail{}, ErrWebhookDeliveryNotFound
	}
	if err != nil {
		return WebhookDeliveryDetail{}, fmt.Errorf("store: get webhook delivery: %w", err)
	}
	return WebhookDeliveryDetail{
		WebhookDeliverySummary: WebhookDeliverySummary{
			ID:         row.ID,
			Provider:   row.Provider,
			Event:      row.Event,
			MaterialID: uuidPtrOrNil(row.MaterialID),
			Status:     row.Status,
			HTTPStatus: row.HttpStatus,
			Error:      stringValue(row.Error),
			ReceivedAt: row.ReceivedAt.Time,
		},
		Headers: row.Headers,
		Payload: row.Payload,
	}, nil
}

// rawOrNull maps an empty RawMessage to nil so pgx stores JSONB
// NULL rather than the literal string "null".
func rawOrNull(b json.RawMessage) []byte {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// uuidPtrOrNil converts a nullable pgtype.UUID into *uuid.UUID.
// Returns nil when the row's material_id was NULL (unmatched
// delivery).
func uuidPtrOrNil(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	v := uuid.UUID(id.Bytes)
	return &v
}
