package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

const secretWebhook = "https://hooks.slack.com/services/T000/B000/SECRETTOKEN0123"

// scrubbed must never emit the token-bearing webhook path, whatever the error
// shape net/http hands back.
func TestScrubbedDropsWebhookToken(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "url.Error (transport failure) echoes the full URL",
			// This is exactly what http.Client.Post returns on a dial failure.
			err: &url.Error{
				Op:  "Post",
				URL: secretWebhook,
				Err: errors.New("dial tcp: lookup hooks.slack.com: no such host"),
			},
		},
		{
			name: "plain error that happens to embed the webhook",
			err:  fmt.Errorf("boom while calling %s", secretWebhook),
		},
		{
			name: "wrapped url.Error",
			err: fmt.Errorf("retry gave up: %w", &url.Error{
				Op:  "Post",
				URL: secretWebhook,
				Err: errors.New("connection refused"),
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scrubbed(tt.err, secretWebhook)
			if strings.Contains(got, "SECRETTOKEN0123") {
				t.Fatalf("scrubbed leaked the webhook token: %q", got)
			}
			if strings.Contains(got, secretWebhook) {
				t.Fatalf("scrubbed leaked the full webhook URL: %q", got)
			}
		})
	}
}

// For the common transport-failure case, the message should still be useful:
// it keeps the operation and the underlying (host-level) cause.
func TestScrubbedKeepsUsefulContext(t *testing.T) {
	err := &url.Error{
		Op:  "Post",
		URL: secretWebhook,
		Err: errors.New("dial tcp: lookup hooks.slack.com: no such host"),
	}
	got := scrubbed(err, secretWebhook)
	if !strings.Contains(got, "Post") || !strings.Contains(got, "no such host") {
		t.Fatalf("scrubbed dropped useful context: %q", got)
	}
}

// With no webhook configured scrubbed must not panic and must pass the message
// through (the replace is a no-op).
func TestScrubbedEmptyWebhook(t *testing.T) {
	if got := scrubbed(errors.New("plain"), ""); got != "plain" {
		t.Fatalf("want %q, got %q", "plain", got)
	}
}
