package domain

import (
	"testing"
	"time"
)

// The precedence table is the whole point of the feature: an operator sets one
// server default and no pipeline author has to touch YAML, while a gate that
// legitimately waits can still opt out.
func TestEffectiveApprovalTimeout(t *testing.T) {
	const week = 168 * time.Hour

	tests := []struct {
		name          string
		spec          *ApprovalSpec
		serverDefault time.Duration
		want          time.Duration
		wantExpires   bool
	}{
		{
			name:          "unset gate inherits the server default",
			spec:          &ApprovalSpec{},
			serverDefault: week,
			want:          week,
			wantExpires:   true,
		},
		{
			name:          "gate window beats the server default",
			spec:          &ApprovalSpec{Timeout: 24 * time.Hour},
			serverDefault: week,
			want:          24 * time.Hour,
			wantExpires:   true,
		},
		{
			name:          "gate window applies with no server default",
			spec:          &ApprovalSpec{Timeout: 24 * time.Hour},
			serverDefault: 0,
			want:          24 * time.Hour,
			wantExpires:   true,
		},
		{
			// The opt-out has to beat the fleet default or it isn't an
			// opt-out — this is the compliance-window / scheduled-release case.
			name:          "never beats the server default",
			spec:          &ApprovalSpec{Timeout: ApprovalTimeoutNever},
			serverDefault: week,
			wantExpires:   false,
		},
		{
			name:          "no gate window and no server default never expires",
			spec:          &ApprovalSpec{},
			serverDefault: 0,
			wantExpires:   false,
		},
		{
			// Guards the "expirer disabled fleet-wide" switch: a negative or
			// zero server default must not be read as "expire immediately".
			name:          "negative server default disables expiry",
			spec:          &ApprovalSpec{},
			serverDefault: -time.Hour,
			wantExpires:   false,
		},
		{
			// A non-gate job carries no ApprovalSpec. The expirer walks
			// definitions that mix gates and regular jobs, so nil must be
			// safe rather than a panic on a hot sweep path.
			name:          "nil spec never expires",
			spec:          nil,
			serverDefault: week,
			wantExpires:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, expires := tt.spec.EffectiveApprovalTimeout(tt.serverDefault)
			if expires != tt.wantExpires {
				t.Fatalf("expires = %v, want %v", expires, tt.wantExpires)
			}
			if expires && got != tt.want {
				t.Fatalf("timeout = %v, want %v", got, tt.want)
			}
		})
	}
}
