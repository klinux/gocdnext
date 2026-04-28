package domain_test

import (
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

func TestEffectiveLogArchive(t *testing.T) {
	tt := []bool{true, false}
	tests := []struct {
		name        string
		policy      domain.LogArchivePolicy
		project     *bool // nil = no override
		hasStore    bool
		want        bool
	}{
		{name: "off-disables-everything", policy: domain.LogArchiveOff, hasStore: true, want: false},
		{name: "off-ignores-project-on", policy: domain.LogArchiveOff, project: &tt[0], hasStore: true, want: false},
		{name: "on-with-store", policy: domain.LogArchiveOn, hasStore: true, want: true},
		{name: "on-without-store-cant-archive", policy: domain.LogArchiveOn, hasStore: false, want: false},
		{name: "on-project-override-off", policy: domain.LogArchiveOn, project: &tt[1], hasStore: true, want: false},
		{name: "auto-with-store", policy: domain.LogArchiveAuto, hasStore: true, want: true},
		{name: "auto-without-store", policy: domain.LogArchiveAuto, hasStore: false, want: false},
		{name: "auto-project-on-with-store", policy: domain.LogArchiveAuto, project: &tt[0], hasStore: true, want: true},
		{name: "auto-project-off-with-store", policy: domain.LogArchiveAuto, project: &tt[1], hasStore: true, want: false},
		{name: "unknown-policy-falls-back-to-auto", policy: "weird", hasStore: true, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.EffectiveLogArchive(tc.policy, tc.project, tc.hasStore); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
