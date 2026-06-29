package analytics

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeStore struct{ calls chan int }

func (f *fakeStore) RefreshRunDaily(_ context.Context, sinceDays int) error {
	f.calls <- sinceDays
	return nil
}

func TestRefresher_BackfillsOnBoot(t *testing.T) {
	f := &fakeStore{calls: make(chan int, 4)}
	r := NewRefresher(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	select {
	case v := <-f.calls:
		if v != 0 {
			t.Fatalf("boot backfill sinceDays = %d, want 0 (all history)", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresher did not backfill on boot")
	}
}
