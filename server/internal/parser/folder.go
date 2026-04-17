package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// ConfigFolderName is the conventional directory inside a repo that holds
// pipeline definitions. One file = one pipeline.
const ConfigFolderName = ".gocdnext"

// LoadFolder reads every *.yaml / *.yml file inside `.gocdnext/` and returns
// the parsed pipelines. Filenames (without extension) are used as the
// pipeline name fallback when the file has no `name:` field.
//
// Returns a stable, sorted result for reproducible diffs in git / UI.
func LoadFolder(root, projectID string) ([]*domain.Pipeline, error) {
	dir := filepath.Join(root, ConfigFolderName)
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
