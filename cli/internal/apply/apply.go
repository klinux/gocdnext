// Package apply implements the `gocdnext apply <path>` logic: collect YAML
// files from a repo's `.gocdnext/` folder and POST them to the server. Kept
// split out of cmd/ so unit tests can exercise the packaging without
// spawning cobra.
package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const configFolderName = ".gocdnext"

// File mirrors the server's projectsapi.ApplyFile. Duplicated on purpose —
// cli is a separate module and cannot import server/internal/*.
type File struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type Request struct {
	Slug        string `json:"slug"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	ConfigRepo  string `json:"config_repo,omitempty"`
	Files       []File `json:"files"`
}

type PipelineStatus struct {
	Name             string `json:"name"`
	PipelineID       string `json:"pipeline_id"`
	Created          bool   `json:"created"`
	MaterialsAdded   int    `json:"materials_added"`
	MaterialsRemoved int    `json:"materials_removed"`
}

type Response struct {
	ProjectID        string           `json:"project_id"`
	ProjectCreated   bool             `json:"project_created"`
	Pipelines        []PipelineStatus `json:"pipelines"`
	PipelinesRemoved []string         `json:"pipelines_removed"`
}

// ReadFolder collects every *.yaml / *.yml file inside `<root>/.gocdnext/`.
// The result is stable-sorted by filename so the server sees the same order
// on each run and its duplicate-name check is deterministic.
func ReadFolder(root string) ([]File, error) {
	dir := filepath.Join(root, configFolderName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var files []File
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch filepath.Ext(name) {
		case ".yaml", ".yml":
		default:
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		files = append(files, File{Name: name, Content: string(content)})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no YAML files in %s", dir)
	}
	return files, nil
}

// Post sends the request to `{serverURL}/api/v1/projects/apply` and returns
// the parsed response. Non-2xx responses include the server's error body.
func Post(ctx context.Context, client *http.Client, serverURL string, req Request) (Response, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("marshal: %w", err)
	}

	u := strings.TrimRight(serverURL, "/") + "/api/v1/projects/apply"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("post %s: %w", u, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out Response
	if err := json.Unmarshal(respBody, &out); err != nil {
		return Response{}, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}
