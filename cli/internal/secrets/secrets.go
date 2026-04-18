// Package secrets is the CLI-side HTTP client for /api/v1/projects/{slug}/secrets.
// Kept split from cmd/ so tests can exercise the request shaping without
// booting cobra.
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type SetRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type SetResponse struct {
	Name    string `json:"name"`
	Created bool   `json:"created"`
}

type Secret struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type ListResponse struct {
	Secrets []Secret `json:"secrets"`
}

// Set posts the plaintext value; server encrypts at rest. Returns
// Created=true on first insert, false on update.
func Set(ctx context.Context, client *http.Client, serverURL, slug string, req SetRequest) (SetResponse, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return SetResponse{}, fmt.Errorf("marshal: %w", err)
	}
	u := endpoint(serverURL, slug, "")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return SetResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return SetResponse{}, fmt.Errorf("post %s: %w", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SetResponse{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out SetResponse
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			return SetResponse{}, fmt.Errorf("decode: %w", err)
		}
	}
	return out, nil
}

// List returns every secret's name + timestamps. Values never cross the wire.
func List(ctx context.Context, client *http.Client, serverURL, slug string) ([]Secret, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	u := endpoint(serverURL, slug, "")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out ListResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out.Secrets, nil
}

// Delete removes a named secret. Returns a clean nil on success.
func Delete(ctx context.Context, client *http.Client, serverURL, slug, name string) error {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	u := endpoint(serverURL, slug, name)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("delete %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

func endpoint(serverURL, slug, name string) string {
	base := strings.TrimRight(serverURL, "/") + "/api/v1/projects/" + url.PathEscape(slug) + "/secrets"
	if name != "" {
		base += "/" + url.PathEscape(name)
	}
	return base
}
