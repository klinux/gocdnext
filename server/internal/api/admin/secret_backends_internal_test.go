package admin

import (
	"errors"
	"testing"
)

func TestClassifyProbe(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil is ok", nil, "ok"},
		{"403 → unauthorized", errors.New("vault: health check: 403 permission denied"), "unauthorized"},
		{"401 → unauthorized", errors.New("aws: 401 unauthorized"), "unauthorized"},
		{"connection refused → unreachable", errors.New("dial tcp 127.0.0.1:1: connection refused"), "unreachable"},
		{"no such host → unreachable", errors.New("lookup vault: no such host"), "unreachable"},
		{"deadline → unreachable", errors.New("context deadline exceeded"), "unreachable"},
		{"other → error", errors.New("something odd"), "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyProbe(tt.err).Status; got != tt.want {
				t.Errorf("classifyProbe(%v).Status = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
