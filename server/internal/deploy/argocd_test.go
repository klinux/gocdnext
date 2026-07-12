package deploy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("mustTime %q: %v", s, err)
	}
	return ts
}

func TestParseApplicationStatus(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want DeployState
	}{
		{
			name: "synced + healthy with revision",
			raw:  `{"status":{"sync":{"status":"Synced","revision":"abc123"},"health":{"status":"Healthy"}}}`,
			want: DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "abc123"},
		},
		{
			name: "outofsync + degraded",
			raw:  `{"status":{"sync":{"status":"OutOfSync","revision":"def456"},"health":{"status":"Degraded"}}}`,
			want: DeployState{Sync: SyncOutOfSync, Health: HealthDegraded, ObservedRev: "def456"},
		},
		{
			name: "empty object → all unknown, no rev",
			raw:  `{}`,
			want: DeployState{Sync: SyncUnknown, Health: HealthUnknown, ObservedRev: ""},
		},
		{
			name: "status present but sync/health missing → unknown",
			raw:  `{"status":{}}`,
			want: DeployState{Sync: SyncUnknown, Health: HealthUnknown},
		},
		{
			// Defends the ADR "tolerate apiVersion / unknown-field drift" corner
			// case: an unrecognized health string maps to Unknown (→ Evaluate treats
			// it as Pending) rather than crashing or being read as a false success.
			name: "unrecognized health value → unknown",
			raw:  `{"status":{"sync":{"status":"Synced"},"health":{"status":"Frobnicated"}}}`,
			want: DeployState{Sync: SyncSynced, Health: HealthUnknown},
		},
		{
			name: "progressing, no revision yet",
			raw:  `{"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Progressing"}}}`,
			want: DeployState{Sync: SyncOutOfSync, Health: HealthProgressing},
		},
		{
			name: "operationState phase Succeeded",
			raw:  `{"status":{"sync":{"status":"Synced","revision":"r"},"health":{"status":"Healthy"},"operationState":{"phase":"Succeeded"}}}`,
			want: DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "r", OperationPhase: OpSucceeded},
		},
		{
			name: "operationState phase Running",
			raw:  `{"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Progressing"},"operationState":{"phase":"Running"}}}`,
			want: DeployState{Sync: SyncOutOfSync, Health: HealthProgressing, OperationPhase: OpRunning},
		},
		{
			name: "unrecognized operation phase → empty",
			raw:  `{"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"operationState":{"phase":"Frobnicating"}}}`,
			want: DeployState{Sync: SyncSynced, Health: HealthHealthy, OperationPhase: ""},
		},
		{
			// Multi-source Application: .sync.revisions present, .sync.revision empty
			// → ObservedRev stays "" (out of scope; fail-closed).
			name: "multi-source revisions → empty observed rev",
			raw:  `{"status":{"sync":{"status":"Synced","revisions":["r1","r2"]},"health":{"status":"Healthy"}}}`,
			want: DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: ""},
		},
		{
			// The correlation anchors: startedAt + syncResult.revision, read for the
			// watch loop to tie an operationState to THIS deploy.
			name: "operationState with startedAt + syncResult revision",
			raw:  `{"status":{"sync":{"status":"Synced","revision":"r"},"health":{"status":"Healthy"},"operationState":{"phase":"Succeeded","startedAt":"2026-01-02T03:04:05Z","syncResult":{"revision":"r"}}}}`,
			want: DeployState{
				Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "r", OperationPhase: OpSucceeded,
				OperationStartedAt: mustTime(t, "2026-01-02T03:04:05Z"), SyncResultRevision: "r",
			},
		},
		{
			// syncResult.revision (what was synced) can differ from the live
			// .sync.revision (what the app tracks now); the loop correlates on the former.
			name: "syncResult revision differs from observed",
			raw:  `{"status":{"sync":{"status":"Synced","revision":"new"},"health":{"status":"Healthy"},"operationState":{"phase":"Succeeded","startedAt":"2026-01-02T03:04:05Z","syncResult":{"revision":"old"}}}}`,
			want: DeployState{
				Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "new", OperationPhase: OpSucceeded,
				OperationStartedAt: mustTime(t, "2026-01-02T03:04:05Z"), SyncResultRevision: "old",
			},
		},
		{
			// A malformed startedAt yields the zero time (loop fails closed) — it must
			// NOT error out the otherwise-valid observation.
			name: "malformed startedAt → zero time, rest still parses",
			raw:  `{"status":{"sync":{"status":"Synced","revision":"r"},"health":{"status":"Healthy"},"operationState":{"phase":"Running","startedAt":"not-a-time","syncResult":{"revision":"r"}}}}`,
			want: DeployState{
				Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "r", OperationPhase: OpRunning,
				SyncResultRevision: "r", // OperationStartedAt is the zero time
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseApplicationStatus([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parseApplicationStatus: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("parseApplicationStatus = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseApplicationStatus_Malformed(t *testing.T) {
	if _, err := parseApplicationStatus([]byte(`{not json`)); err == nil {
		t.Fatal("expected an error decoding malformed JSON, got nil")
	}
}

// fakeFetcher stands in for the (later) k8s-CRD / ArgoCD-API transport so Observe
// is exercised without a real cluster.
type fakeFetcher struct {
	raw []byte
	err error
}

func (f fakeFetcher) fetchApplication(context.Context, DeploymentTarget) ([]byte, error) {
	return f.raw, f.err
}

func TestObserve(t *testing.T) {
	target := DeploymentTarget{Provider: "argocd", Cluster: "prod", Application: "checkout", Namespace: "argocd"}

	t.Run("parses the fetched application", func(t *testing.T) {
		p := newArgoProviderWith(fakeFetcher{raw: []byte(`{"status":{"sync":{"status":"Synced","revision":"r1"},"health":{"status":"Healthy"}}}`)})
		got, err := p.Observe(context.Background(), target)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		want := DeployState{Sync: SyncSynced, Health: HealthHealthy, ObservedRev: "r1"}
		if got != want {
			t.Errorf("Observe = %+v, want %+v", got, want)
		}
	})

	t.Run("wraps a fetch error", func(t *testing.T) {
		sentinel := errors.New("boom")
		p := newArgoProviderWith(fakeFetcher{err: sentinel})
		if _, err := p.Observe(context.Background(), target); !errors.Is(err, sentinel) {
			t.Fatalf("Observe error = %v, want it to wrap %v", err, sentinel)
		}
	})
}
