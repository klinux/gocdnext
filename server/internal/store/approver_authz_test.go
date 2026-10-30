package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// oidcUser upserts a user the way an OIDC login does: provider + the IdP's
// stable `sub` as external_id.
func oidcUser(t *testing.T, s *store.Store, sub, email, name string) store.User {
	t.Helper()
	u, err := s.UpsertUserByProvider(context.Background(), store.UpsertUserInput{
		Email: email, Name: name, Provider: "oidc", ExternalID: sub,
	})
	if err != nil {
		t.Fatalf("upsert %s: %v", email, err)
	}
	return u
}

func decisionFor(gate uuid.UUID, u store.User) store.ApprovalDecision {
	return store.ApprovalDecision{
		JobRunID: gate, UserID: u.ID, User: u.Name, UserEmail: u.Email,
		Provider: u.Provider, Subject: u.ExternalID,
	}
}

func gateStatus(t *testing.T, pool *pgxpool.Pool, gate uuid.UUID) (status string, votes int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT j.status, (SELECT COUNT(*) FROM job_run_approvals a WHERE a.job_run_id = j.id)
		FROM job_runs j WHERE j.id = $1`, gate).Scan(&status, &votes); err != nil {
		t.Fatalf("gate status: %v", err)
	}
	return
}

// #172: a platform approving with a service token turns the gate into theatre —
// quorum counts the machine and the audit names no human. Refused regardless of
// the allow-list (including the permissive empty one), before any vote lands.
func TestApproveGate_RefusesServiceAccount(t *testing.T) {
	tests := []struct {
		name      string
		approvers []string
	}{
		{"permissive empty allow-list", nil},
		{"SA named like a listed approver", []string{"deploy-bot"}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := dbtest.SetupPool(t)
			s := store.New(pool)
			pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-sa-"+string(rune('a'+i)), tt.approvers)
			_, gate := triggerApprovalRun(t, pool, pipelineID, materialID)

			for _, decide := range []func(context.Context, store.ApprovalDecision) (store.ApprovalResult, error){s.ApproveGate, s.RejectGate} {
				_, err := decide(context.Background(), store.ApprovalDecision{
					JobRunID: gate, UserID: uuid.New(), User: "deploy-bot",
					UserEmail: "deploy-bot@service-account", Provider: store.ProviderServiceAccount,
				})
				if !errors.Is(err, store.ErrApproverNotHuman) {
					t.Fatalf("err = %v, want ErrApproverNotHuman", err)
				}
			}
			if status, votes := gateStatus(t, pool, gate); status != "awaiting_approval" || votes != 0 {
				t.Fatalf("gate = %s with %d votes, want untouched", status, votes)
			}
		})
	}
}

func TestApproveGate_MatchesSubjectRef(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-sub", []string{"sub:oidc:sub-alice"})
	_, gate := triggerApprovalRun(t, pool, pipelineID, materialID)

	alice := oidcUser(t, s, "sub-alice", "alice@corp.com", "Alice")
	// Renamed + new email since the gate was written: the subject still matches.
	alice.Name, alice.Email = "Alice Married-Name", "alice.mn@corp.com"
	if _, err := s.ApproveGate(context.Background(), decisionFor(gate, alice)); err != nil {
		t.Fatalf("approve by subject: %v", err)
	}
	if status, _ := gateStatus(t, pool, gate); status != "success" {
		t.Fatalf("status = %s, want success", status)
	}
}

// A subject entry matches ONLY by subject. A user whose display label (or
// email) spells the victim's subject ref must not satisfy it.
func TestApproveGate_SubjectRefNeverMatchesByLabel(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-sub-spoof", []string{"sub:oidc:sub-alice"})
	_, gate := triggerApprovalRun(t, pool, pipelineID, materialID)

	mallory := oidcUser(t, s, "sub-mallory", "mallory@corp.com", "sub:oidc:sub-alice")
	d := decisionFor(gate, mallory)
	d.UserEmail = "sub:oidc:sub-alice"
	if _, err := s.ApproveGate(context.Background(), d); !errors.Is(err, store.ErrApproverNotAllowed) {
		t.Fatalf("err = %v, want ErrApproverNotAllowed", err)
	}
	// Same subject under a DIFFERENT provider is a different person.
	d = decisionFor(gate, mallory)
	d.Provider, d.Subject = "github", "sub-alice"
	if _, err := s.ApproveGate(context.Background(), d); !errors.Is(err, store.ErrApproverNotAllowed) {
		t.Fatalf("cross-provider err = %v, want ErrApproverNotAllowed", err)
	}
}

// fakeGroupSource stands in for an external (IdP/platform) membership source.
type fakeGroupSource struct {
	groups map[string][]string // keyed by subject
	err    error
	calls  int
	seen   store.ApproverIdentity
}

func (f *fakeGroupSource) ApproverGroupNames(_ context.Context, who store.ApproverIdentity) ([]string, error) {
	f.calls++
	f.seen = who
	if f.err != nil {
		return nil, f.err
	}
	return f.groups[who.Subject], nil
}

func groupGate(t *testing.T, pool *pgxpool.Pool, slug string, groups []string) uuid.UUID {
	t.Helper()
	pipelineID, materialID := seedApprovalPipeline(t, pool, slug, nil)
	_, gate := triggerApprovalRun(t, pool, pipelineID, materialID)
	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET approver_groups = $2 WHERE id = $1`, gate, groups); err != nil {
		t.Fatalf("set approver_groups: %v", err)
	}
	return gate
}

func TestApproveGate_ExternalGroupSource(t *testing.T) {
	t.Run("grants membership resolved at vote time", func(t *testing.T) {
		pool := dbtest.SetupPool(t)
		src := &fakeGroupSource{groups: map[string][]string{"sub-alice": {"platform-sre"}}}
		s := store.New(pool).WithApproverGroupSource(src)
		gate := groupGate(t, pool, "gate-ext-grant", []string{"platform-sre"})
		dave := oidcUser(t, s, "sub-dave", "dave@corp.com", "Dave")
		alice := oidcUser(t, s, "sub-alice", "alice@corp.com", "Alice")

		if _, err := s.ApproveGate(context.Background(), decisionFor(gate, dave)); !errors.Is(err, store.ErrApproverNotAllowed) {
			t.Fatalf("non-member err = %v, want ErrApproverNotAllowed", err)
		}
		if _, err := s.ApproveGate(context.Background(), decisionFor(gate, alice)); err != nil {
			t.Fatalf("external member approve: %v", err)
		}
		if src.seen.Provider != "oidc" || src.seen.Subject != "sub-alice" || src.seen.UserID != alice.ID {
			t.Fatalf("source saw %+v, want alice's provider/subject/id", src.seen)
		}
	})

	t.Run("resolution failure fails closed", func(t *testing.T) {
		pool := dbtest.SetupPool(t)
		src := &fakeGroupSource{err: errors.New("platform down")}
		s := store.New(pool).WithApproverGroupSource(src)
		gate := groupGate(t, pool, "gate-ext-down", []string{"platform-sre"})
		alice := oidcUser(t, s, "sub-alice", "alice@corp.com", "Alice")

		if _, err := s.ApproveGate(context.Background(), decisionFor(gate, alice)); !errors.Is(err, store.ErrApproverGroupsUnavailable) {
			t.Fatalf("err = %v, want ErrApproverGroupsUnavailable", err)
		}
		if status, votes := gateStatus(t, pool, gate); status != "awaiting_approval" || votes != 0 {
			t.Fatalf("gate = %s with %d votes, want untouched", status, votes)
		}
	})

	t.Run("local membership decides without calling the source", func(t *testing.T) {
		pool := dbtest.SetupPool(t)
		src := &fakeGroupSource{err: errors.New("platform down")}
		s := store.New(pool).WithApproverGroupSource(src)
		gate := groupGate(t, pool, "gate-ext-local", []string{"sre"})
		alice := oidcUser(t, s, "sub-alice", "alice@corp.com", "Alice")
		grp, err := s.InsertGroup(context.Background(), store.GroupInput{Name: "sre"})
		if err != nil {
			t.Fatalf("insert group: %v", err)
		}
		if err := s.AddGroupMember(context.Background(), grp.ID, alice.ID, nil); err != nil {
			t.Fatalf("add member: %v", err)
		}

		if _, err := s.ApproveGate(context.Background(), decisionFor(gate, alice)); err != nil {
			t.Fatalf("local member approve: %v", err)
		}
		if src.calls != 0 {
			t.Fatalf("source called %d times, want 0 (local table already granted)", src.calls)
		}
	})
}

// The rollout engine shares the authorizer: a service account is refused and a
// subject ref matches, same as the job gate.
func TestDecideRolloutGate_IdentityRules(t *testing.T) {
	gr := seedGatedRollout(t, "rg-identity", 1, []string{"sub:oidc:sub-alice"})
	alice := oidcUser(t, gr.s, "sub-alice", "alice@corp.com", "Alice")

	in := store.RolloutGateDecisionInput{
		RevisionID: gr.revID, ProjectID: gr.projectID, GateID: gr.gateID, Decision: "approved",
		UserID: uuid.New(), User: "deploy-bot", UserEmail: "deploy-bot@service-account",
		Provider: store.ProviderServiceAccount,
	}
	if _, err := gr.s.DecideRolloutGate(gr.ctx, in); !errors.Is(err, store.ErrApproverNotHuman) {
		t.Fatalf("service account err = %v, want ErrApproverNotHuman", err)
	}

	in.UserID, in.User, in.UserEmail = alice.ID, "Alice Renamed", "new@corp.com"
	in.Provider, in.Subject = alice.Provider, alice.ExternalID
	res, err := gr.s.DecideRolloutGate(gr.ctx, in)
	if err != nil || !res.Decided {
		t.Fatalf("subject approve = %+v err=%v, want Decided", res, err)
	}
}
