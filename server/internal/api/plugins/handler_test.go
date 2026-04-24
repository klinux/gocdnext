package plugins_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	apiplugins "github.com/gocdnext/gocdnext/server/internal/api/plugins"
	plugcat "github.com/gocdnext/gocdnext/server/internal/plugins"
)

func TestList_EmptyCatalogReturnsEmptyArray(t *testing.T) {
	// Nil + empty catalog must both serialize `{"plugins": []}`
	// — a null/missing field would force the UI to special-case.
	cases := map[string]*plugcat.Catalog{
		"nil":   nil,
		"empty": plugcat.New(),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := apiplugins.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), c)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
			rr := httptest.NewRecorder()
			h.List(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
			}
			var resp struct {
				Plugins []any `json:"plugins"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v body=%s", err, rr.Body.String())
			}
			if resp.Plugins == nil {
				t.Error("plugins field is null; must be empty array")
			}
			if len(resp.Plugins) != 0 {
				t.Errorf("plugins = %+v, want empty", resp.Plugins)
			}
		})
	}
}

func TestList_SerializesSpecsInSortedOrder(t *testing.T) {
	c := plugcat.New()
	c.Register(plugcat.Spec{
		Name:        "node",
		Description: "pnpm helper",
		Inputs: map[string]plugcat.Input{
			"working-dir": {Required: false, Default: ".", Description: "subdir"},
			"command":     {Required: true, Description: "pnpm cmd"},
		},
	})
	c.Register(plugcat.Spec{
		Name:        "go",
		Description: "go build/test",
		Inputs: map[string]plugcat.Input{
			"command": {Required: true, Description: "go subcmd"},
		},
	})

	h := apiplugins.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), c)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	var resp struct {
		Plugins []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Inputs      []struct {
				Name        string `json:"name"`
				Required    bool   `json:"required"`
				Default     string `json:"default"`
				Description string `json:"description"`
			} `json:"inputs"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if len(resp.Plugins) != 2 || resp.Plugins[0].Name != "go" || resp.Plugins[1].Name != "node" {
		t.Fatalf("plugin order wrong: %+v", resp.Plugins)
	}
	// Inputs must also be sorted so docs are stable.
	node := resp.Plugins[1]
	if len(node.Inputs) != 2 || node.Inputs[0].Name != "command" || node.Inputs[1].Name != "working-dir" {
		t.Errorf("input order wrong: %+v", node.Inputs)
	}
	// Required-ness + default + description all round-trip.
	if !node.Inputs[0].Required {
		t.Errorf("command should be required")
	}
	if node.Inputs[1].Default != "." {
		t.Errorf("working-dir default = %q", node.Inputs[1].Default)
	}
}

func TestList_RejectsNonGET(t *testing.T) {
	h := apiplugins.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), plugcat.New())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}
