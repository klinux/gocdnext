package store

import (
	"context"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// runnerProfilesFile is the YAML schema the boot-time seeder reads.
// Mirrors the admin write API one-for-one so a Helm-managed file
// and a UI-edited row are interchangeable.
type runnerProfilesFile struct {
	Profiles []runnerProfileEntry `yaml:"profiles"`
}

type runnerProfileEntry struct {
	Name              string         `yaml:"name"`
	Description       string         `yaml:"description"`
	Engine            string         `yaml:"engine"`
	DefaultImage      string         `yaml:"default_image"`
	DefaultCPURequest string         `yaml:"default_cpu_request"`
	DefaultCPULimit   string         `yaml:"default_cpu_limit"`
	DefaultMemRequest string         `yaml:"default_mem_request"`
	DefaultMemLimit   string         `yaml:"default_mem_limit"`
	MaxCPU            string         `yaml:"max_cpu"`
	MaxMem            string         `yaml:"max_mem"`
	Tags              []string       `yaml:"tags"`
	Config            map[string]any `yaml:"config"`
	// Env carries plaintext, non-secret runtime config (bucket
	// names, regions, GOCDNEXT_LAYER_CACHE_* defaults). Plain
	// values.yaml is the right place — they're not credentials.
	// Secrets are deliberately NOT seeded from this YAML: a values
	// file commonly lives in git, plaintext credentials there are
	// a foot-gun. Use the admin UI (or sealed-secrets) to manage
	// `secrets:` post-install.
	Env map[string]string `yaml:"env"`
}

// SeedRunnerProfilesFromFile reads a YAML file and upserts each
// profile entry by name. New names insert; existing names update
// in place; profiles in the DB but not in the file are LEFT ALONE
// (operators may have created ad-hoc rows in the UI). Idempotent —
// running twice has the same effect as running once.
//
// Returns the number of rows touched (created + updated). A non-nil
// error aborts startup so a typo in the YAML doesn't ship a partial
// catalogue.
func (s *Store) SeedRunnerProfilesFromFile(ctx context.Context, path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("seed runner profiles: read %s: %w", path, err)
	}
	var file runnerProfilesFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return 0, fmt.Errorf("seed runner profiles: parse %s: %w", path, err)
	}
	touched := 0
	for i, p := range file.Profiles {
		if p.Name == "" {
			return touched, fmt.Errorf("seed runner profiles: entry %d: name is required", i)
		}
		if p.Engine == "" {
			return touched, fmt.Errorf("seed runner profiles: entry %q: engine is required", p.Name)
		}
		input := RunnerProfileInput{
			Name:              p.Name,
			Description:       p.Description,
			Engine:            p.Engine,
			DefaultImage:      p.DefaultImage,
			DefaultCPURequest: p.DefaultCPURequest,
			DefaultCPULimit:   p.DefaultCPULimit,
			DefaultMemRequest: p.DefaultMemRequest,
			DefaultMemLimit:   p.DefaultMemLimit,
			MaxCPU:            p.MaxCPU,
			MaxMem:            p.MaxMem,
			Tags:              p.Tags,
			Config:            p.Config,
			Env:               p.Env,
			// Secrets intentionally NOT seeded from YAML — see the
			// type comment for the rationale.
		}
		existing, err := s.GetRunnerProfileByName(ctx, p.Name)
		switch {
		case errors.Is(err, ErrRunnerProfileNotFound):
			// Seed profiles never carry secrets — nil cipher is
			// safe; encodeProfileSecrets fast-paths empty input.
			if _, err := s.InsertRunnerProfile(ctx, nil, input); err != nil {
				return touched, fmt.Errorf("seed runner profiles: insert %q: %w", p.Name, err)
			}
		case err != nil:
			return touched, fmt.Errorf("seed runner profiles: lookup %q: %w", p.Name, err)
		default:
			// UpdateRunnerProfileFromSeed leaves the `secrets`
			// column alone — declarative seed handles env +
			// resources + tags; admin UI handles secrets. No
			// double-management surprise on reboot.
			if err := s.UpdateRunnerProfileFromSeed(ctx, existing.ID, input); err != nil {
				return touched, fmt.Errorf("seed runner profiles: update %q: %w", p.Name, err)
			}
		}
		touched++
	}
	return touched, nil
}
