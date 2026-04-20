package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// statusCapture wraps http.ResponseWriter so the webhook delivery
// audit row can report the final HTTP status without the handlers
// having to pass it around.
type statusCapture struct {
	http.ResponseWriter
	status int
}

func (s *statusCapture) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapture) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// auditedHeaders is the list of request headers we persist with
// each webhook delivery. The signed body already contains the
// interesting payload; we only need identifying/audit headers so
// the admin drawer can show delivery id, event type, and signing
// data.
var auditedHeaders = []string{
	"X-GitHub-Event",
	"X-GitHub-Delivery",
	"X-GitHub-Hook-Id",
	"X-GitHub-Hook-Installation-Target-Id",
	"X-GitHub-Hook-Installation-Target-Type",
	"X-Hub-Signature-256",
	"Content-Type",
	"User-Agent",
}

func headersJSON(h http.Header) json.RawMessage {
	picked := map[string]string{}
	for _, name := range auditedHeaders {
		if v := h.Get(name); v != "" {
			picked[name] = v
		}
	}
	b, _ := json.Marshal(picked)
	return json.RawMessage(b)
}

// deliveryRec accumulates fields written to webhook_deliveries at
// the end of each handler. Handlers set status/errText/materialID
// as they progress; the deferred recordDelivery call flushes it.
type deliveryRec struct {
	provider   string
	event      string
	materialID uuid.UUID
	status     string
	errText    string
	headers    json.RawMessage
	payload    json.RawMessage
	writer     *statusCapture
}

func (h *Handler) recordDelivery(ctx context.Context, rec *deliveryRec) {
	if h.store == nil || rec == nil {
		return
	}
	httpStatus := int32(rec.writer.status)
	if httpStatus == 0 {
		httpStatus = http.StatusOK
	}
	status := rec.status
	if status == "" {
		switch {
		case httpStatus >= 200 && httpStatus < 300:
			status = store.WebhookStatusAccepted
		case httpStatus == http.StatusUnauthorized:
			status = store.WebhookStatusRejected
		default:
			status = store.WebhookStatusError
		}
	}
	if _, _, err := h.store.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryInput{
		Provider:   rec.provider,
		Event:      rec.event,
		MaterialID: rec.materialID,
		Status:     status,
		HTTPStatus: httpStatus,
		Headers:    rec.headers,
		Payload:    rec.payload,
		Error:      rec.errText,
	}); err != nil {
		h.log.Warn("webhook: audit insert failed",
			"provider", rec.provider, "event", rec.event, "err", err)
	}
}
