package store_test

import (
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestNotificationTriggerMatches(t *testing.T) {
	cases := []struct {
		name string
		on   domain.NotificationTrigger
		in   store.UserStageOutcome
		want bool
	}{
		{"always clean run", domain.NotifyOnAlways, store.UserStageOutcome{}, true},
		{"always failed run", domain.NotifyOnAlways, store.UserStageOutcome{Failed: 1}, true},

		{"failure — no failures → no fire", domain.NotifyOnFailure, store.UserStageOutcome{}, false},
		{"failure — some failed → fire", domain.NotifyOnFailure, store.UserStageOutcome{Failed: 2}, true},
		{"failure — only canceled → no fire",
			domain.NotifyOnFailure, store.UserStageOutcome{Canceled: 1}, false},

		{"success — clean → fire", domain.NotifyOnSuccess, store.UserStageOutcome{}, true},
		{"success — any failure → no fire",
			domain.NotifyOnSuccess, store.UserStageOutcome{Failed: 1}, false},
		{"success — any cancel → no fire",
			domain.NotifyOnSuccess, store.UserStageOutcome{Canceled: 1}, false},

		{"canceled — none → no fire", domain.NotifyOnCanceled, store.UserStageOutcome{}, false},
		{"canceled — had cancel → fire",
			domain.NotifyOnCanceled, store.UserStageOutcome{Canceled: 1}, true},
		{"canceled — had failure only → no fire",
			domain.NotifyOnCanceled, store.UserStageOutcome{Failed: 3}, false},

		{"unknown trigger → no fire",
			domain.NotificationTrigger("whatever"), store.UserStageOutcome{Failed: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := store.NotificationTriggerMatches(c.on, c.in)
			if got != c.want {
				t.Errorf("want %v got %v (on=%q in=%+v)", c.want, got, c.on, c.in)
			}
		})
	}
}

func TestNotificationJobName_RoundTrip(t *testing.T) {
	for _, idx := range []int{0, 1, 42, 9999} {
		name := domain.NotificationJobName(idx)
		if !domain.IsNotificationJobName(name) {
			t.Errorf("IsNotificationJobName(%q) = false", name)
		}
		back, ok := domain.NotificationIndexFromName(name)
		if !ok || back != idx {
			t.Errorf("round trip idx=%d → %q → (%d, %v)", idx, name, back, ok)
		}
	}
}

func TestNotificationIndexFromName_RejectsNoise(t *testing.T) {
	for _, name := range []string{
		"", "build", "_notify_", "_notify_abc", "_notify_-1", "notify_0",
	} {
		if _, ok := domain.NotificationIndexFromName(name); ok {
			t.Errorf("%q wrongly parsed as a notification name", name)
		}
	}
}
