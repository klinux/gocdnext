package scheduler

import (
	"errors"
	"testing"
)

func TestCorrelationRevision(t *testing.T) {
	const full = "aaa0123456789aaa0123456789aaa0123456789a" // 40 hex, the run's commit

	tests := []struct {
		name     string
		display  string
		explicit bool
		fullSHA  string
		want     string
		wantErr  bool
	}{
		{name: "default uses the run's full commit", display: "aaa0123", explicit: false, fullSHA: full, want: full},
		{name: "explicit full SHA used directly", display: full, explicit: true, fullSHA: full, want: full},
		{name: "explicit full SHA of a DIFFERENT commit is honored", display: "bbb0123456789bbb0123456789bbb0123456789b", explicit: true, fullSHA: full, want: "bbb0123456789bbb0123456789bbb0123456789b"},
		{name: "explicit uppercase full SHA is lowercased", display: "AAA0123456789AAA0123456789AAA0123456789A", explicit: true, fullSHA: full, want: full},
		{name: "explicit short SHA prefixing the commit expands to full", display: "aaa0123", explicit: true, fullSHA: full, want: full},
		{name: "explicit short SHA NOT prefixing the commit is not correlatable", display: "deadbee", explicit: true, fullSHA: full, wantErr: true},
		{name: "explicit semver is not correlatable", display: "1.2.3", explicit: true, fullSHA: full, wantErr: true},
		{name: "explicit tag is not correlatable", display: "v1.0.0", explicit: true, fullSHA: full, wantErr: true},
		{name: "explicit full SHA with no run commit still correlates", display: full, explicit: true, fullSHA: "", want: full},
		{name: "explicit short SHA with no run commit cannot expand", display: "aaa0123", explicit: true, fullSHA: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := correlationRevision("ship", tt.display, tt.explicit, tt.fullSHA)
			if tt.wantErr {
				if !errors.Is(err, ErrDeployVersionNotCorrelatable) {
					t.Fatalf("err = %v, want ErrDeployVersionNotCorrelatable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("correlationRevision = %q, want %q", got, tt.want)
			}
		})
	}
}
