package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// DefaultConfigFolder is the convention applied when a caller
// doesn't pass an explicit per-project path. Projects can override
// via projects.config_path (UI.10.b).
const DefaultConfigFolder = ".gocdnext"

// LoadFolder reads every *.yaml / *.yml file inside <root>/<path>
// and returns the parsed pipelines. Filenames (without extension)
// are used as the pipeline name fallback when the file has no
// `name:` field. An empty configPath resolves to DefaultConfigFolder.
//
// When configPath ends in .yaml/.yml it's treated as a single-file
// config (GitLab CI style: one .gocdnext.yml at the root). The
// same parser runs on the one file; "duplicate name" detection is
// a no-op since there's only one.
//
// Returns a stable, sorted result for reproducible diffs in git / UI.
func LoadFolder(root, configPath, projectID string) ([]*domain.Pipeline, error) {
	if configPath == "" {
		configPath = DefaultConfigFolder
	}
	target := filepath.Join(root, configPath)

	if IsSingleFileConfigPath(configPath) {
		fallback := strings.TrimSuffix(filepath.Base(configPath), filepath.Ext(configPath))
		p, err := parseFile(target, projectID, fallback)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", configPath, err)
		}
		return []*domain.Pipeline{p}, nil
	}

	dir := target
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("readdir %s: %w", dir, err)
	}

	var pipelines []*domain.Pipeline
	seen := map[string]string{} // name → file (dup detection)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !hasPipelineExt(name) {
			continue
		}

		path := filepath.Join(dir, name)
		fallbackName := strings.TrimSuffix(name, filepath.Ext(name))

		p, err := parseFile(path, projectID, fallbackName)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if prev, dup := seen[p.Name]; dup {
			return nil, fmt.Errorf("pipeline %q defined twice: %s and %s", p.Name, prev, name)
		}
		seen[p.Name] = name
		pipelines = append(pipelines, p)
	}

	sort.Slice(pipelines, func(i, j int) bool { return pipelines[i].Name < pipelines[j].Name })
	return pipelines, nil
}

func parseFile(path, projectID, fallbackName string) (*domain.Pipeline, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseNamed(f, projectID, fallbackName)
}

func hasPipelineExt(name string) bool {
	switch filepath.Ext(name) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// IsSingleFileConfigPath returns true when the config path points
// at a single YAML file rather than a folder — i.e. ends in
// .yaml or .yml. Exported so both the local parser and the
// remote fetcher (configsync/github.go) classify the path the
// same way without duplicating the decision.
func IsSingleFileConfigPath(path string) bool {
	return hasPipelineExt(path)
}
