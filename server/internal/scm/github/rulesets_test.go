package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/scm/github"
)

func rulesetInput() github.RulesetInput {
	return github.RulesetInput{
		Owner: "org", Repo: "repo",
		Name:     github.RequiredChecksRulesetName("svc"),
		Contexts: []string{"ci/gocdnext/svc/build", "ci/gocdnext/svc/e2e"},
	}
}

func TestUpsertRulesetCreate(t *testing.T) {
	var body map[string]any
	var postSeen bool
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/rulesets":
			// No existing gocdnext ruleset to adopt.
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/repo/rulesets":
			postSeen = true
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242})
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
	})

	id, err := c.UpsertRequiredChecksRuleset(context.Background(), 100, rulesetInput(), nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != 4242 || !postSeen {
		t.Fatalf("id=%d postSeen=%v, want 4242 + POST", id, postSeen)
	}
	if body["name"] != github.RequiredChecksRulesetName("svc") {
		t.Errorf("name = %v, want per-project %q", body["name"], github.RequiredChecksRulesetName("svc"))
	}
	// Targets the default branch (rename-proof), not a literal ref.
	cond, _ := body["conditions"].(map[string]any)
	ref, _ := cond["ref_name"].(map[string]any)
	inc, _ := ref["include"].([]any)
	if len(inc) != 1 || inc[0] != "~DEFAULT_BRANCH" {
		t.Errorf("ref include = %v, want [~DEFAULT_BRANCH]", inc)
	}
	// Each required check is pinned to the gocdnext App (integration_id = appID=1).
	rules, _ := body["rules"].([]any)
	params, _ := rules[0].(map[string]any)["parameters"].(map[string]any)
	rscs, _ := params["required_status_checks"].([]any)
	if len(rscs) != 2 {
		t.Fatalf("required checks = %v", rscs)
	}
	first, _ := rscs[0].(map[string]any)
	if first["context"] != "ci/gocdnext/svc/build" {
		t.Errorf("first context = %v", first["context"])
	}
	if first["integration_id"] != float64(1) {
		t.Errorf("integration_id = %v, want 1 (pins the App as the only satisfier)", first["integration_id"])
	}
}

func TestUpsertRulesetAdoptsExistingByName(t *testing.T) {
	var putPath string
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/rulesets":
			// An existing ruleset with OUR name (e.g. after a DB restore).
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 88, "name": "someone-elses"},
				{"id": 91, "name": github.RequiredChecksRulesetName("svc")},
			})
		case r.Method == http.MethodPut:
			putPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 91})
		default:
			t.Errorf("unexpected call: %s %s (should adopt via PUT, not POST)", r.Method, r.URL.Path)
		}
	})

	id, err := c.UpsertRequiredChecksRuleset(context.Background(), 100, rulesetInput(), nil)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id != 91 || putPath != "/repos/org/repo/rulesets/91" {
		t.Fatalf("adopt = id %d path %q, want 91 + PUT /rulesets/91", id, putPath)
	}
}

func TestUpsertRulesetUpdateInPlace(t *testing.T) {
	var method, path string
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 77})
	})

	existing := int64(77)
	id, err := c.UpsertRequiredChecksRuleset(context.Background(), 100, rulesetInput(), &existing)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if id != 77 || method != http.MethodPut || path != "/repos/org/repo/rulesets/77" {
		t.Fatalf("update = id %d %s %s, want 77 PUT /rulesets/77", id, method, path)
	}
}

func TestUpsertRulesetUpdate404FallsBackToCreate(t *testing.T) {
	var calls []string
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 900})
	})

	existing := int64(555)
	id, err := c.UpsertRequiredChecksRuleset(context.Background(), 100, rulesetInput(), &existing)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id != 900 {
		t.Errorf("recreated id = %d, want 900", id)
	}
	if len(calls) != 2 || calls[0] != "PUT /repos/org/repo/rulesets/555" || calls[1] != "POST /repos/org/repo/rulesets" {
		t.Errorf("calls = %v (expected PUT 404 then POST)", calls)
	}
}

func TestUpsertRuleset403IsTypedAdminError(t *testing.T) {
	// 403 on any call (here the adopt-lookup) is the missing-admin signal.
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
	})

	_, err := c.UpsertRequiredChecksRuleset(context.Background(), 100, rulesetInput(), nil)
	if !errors.Is(err, github.ErrAppLacksAdmin) {
		t.Fatalf("expected ErrAppLacksAdmin, got %v", err)
	}
}

func TestDeleteRulesetOK(t *testing.T) {
	var method, path string
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteRuleset(context.Background(), 100, "org", "repo", 12); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if method != http.MethodDelete || path != "/repos/org/repo/rulesets/12" {
		t.Errorf("unexpected call: %s %s", method, path)
	}
}

func TestDeleteRuleset404IsSuccess(t *testing.T) {
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.DeleteRuleset(context.Background(), 100, "org", "repo", 12); err != nil {
		t.Fatalf("404 delete should be success, got %v", err)
	}
}
