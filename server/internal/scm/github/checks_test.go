package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/scm/github"
)

func TestCreateCheckRun(t *testing.T) {
	var captured atomic.Pointer[map[string]any]
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/org/repo/check-runs" {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured.Store(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       777,
			"status":   body["status"],
			"html_url": "https://github.com/org/repo/runs/777",
		})
	})

	got, err := c.CreateCheckRun(context.Background(), 100, github.CreateCheckRunInput{
		Owner:      "org",
		Repo:       "repo",
		Name:       "gocdnext / ci",
		HeadSHA:    "9f7c3d2e1b8a5f6c4e0d7a9b1c3d5e7f9a0b2c4d",
		Status:     github.CheckStatusInProgress,
		DetailsURL: "https://gocdnext.dev/runs/abc",
		ExternalID: "abc",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.ID != 777 {
		t.Errorf("id = %d", got.ID)
	}

	ptr := captured.Load()
	if ptr == nil {
		t.Fatal("no body captured")
	}
	body := *ptr
	if body["name"] != "gocdnext / ci" {
		t.Errorf("name = %v", body["name"])
	}
	if body["head_sha"] != "9f7c3d2e1b8a5f6c4e0d7a9b1c3d5e7f9a0b2c4d" {
		t.Errorf("head_sha = %v", body["head_sha"])
	}
	if body["status"] != string(github.CheckStatusInProgress) {
		t.Errorf("status = %v", body["status"])
	}
	if body["details_url"] != "https://gocdnext.dev/runs/abc" {
		t.Errorf("details_url = %v", body["details_url"])
	}
}

func TestCreateCheckRun_RejectsMissingFields(t *testing.T) {
	c, _ := appClientWith(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no API call should happen")
	})
	_, err := c.CreateCheckRun(context.Background(), 100, github.CreateCheckRunInput{
		Owner: "org", Repo: "repo",
	})
	if err == nil {
		t.Error("expected error for missing Name+HeadSHA")
	}
}

func TestUpdateCheckRun_Completed(t *testing.T) {
	var captured atomic.Pointer[map[string]any]
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/check-runs/777") {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured.Store(&body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	err := c.UpdateCheckRun(context.Background(), 100, github.UpdateCheckRunInput{
		Owner: "org", Repo: "repo",
		CheckRunID: 777,
		Status:     github.CheckStatusCompleted,
		Conclusion: github.CheckConclusionSuccess,
		Output: &github.CheckRunOutput{
			Title:   "ci passed",
			Summary: "4 jobs, 2 stages",
		},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	body := *captured.Load()
	if body["status"] != string(github.CheckStatusCompleted) {
		t.Errorf("status = %v", body["status"])
	}
	if body["conclusion"] != string(github.CheckConclusionSuccess) {
		t.Errorf("conclusion = %v", body["conclusion"])
	}
	out, _ := body["output"].(map[string]any)
	if out["title"] != "ci passed" {
		t.Errorf("output title = %v", out["title"])
	}
}

func TestUpdateCheckRun_RejectsMissingID(t *testing.T) {
	c, _ := appClientWith(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no API call should happen")
	})
	err := c.UpdateCheckRun(context.Background(), 100, github.UpdateCheckRunInput{
		Owner: "org", Repo: "repo",
	})
	if err == nil {
		t.Error("expected error for missing CheckRunID")
	}
}
