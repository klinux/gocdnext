package vcs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gocdnext/gocdnext/server/internal/config"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// DBSource is the minimal contract the bootstrap needs from the
// store. Kept as an interface so tests can plug a fake without
// standing up a pgx pool.
type DBSource interface {
	ListBootstrapVCSIntegrations(ctx context.Context) ([]store.BootstrapVCSIntegration, error)
}

// BuildRegistry merges env-based GitHub App config with DB-stored
// VCS integrations and returns a populated Registry. Resolution
// rules:
//
//   1. Env config (GOCDNEXT_GITHUB_APP_ID + private key) seeds a
//      "env" integration. Kept even when DB rows exist, so an
//      admin can see what the bootstrap was.
//   2. DB rows are appended. First-seen position wins on display;
//      name collision resolves to DB (later entry).
//   3. The active GitHubApp() client is the FIRST enabled
//      github_app in final iteration order. That lets DB
//      configuration override env when the admin sets up a real
//      app via the UI.
//
// A nil dbSource is valid (DB unreachable at boot); env-only
// loads cleanly.
func BuildRegistry(ctx context.Context, cfg *config.Config, dbSource DBSource, log *slog.Logger) (*Registry, error) {
	reg := New()
	if err := Reload(ctx, reg, cfg, dbSource, log); err != nil {
		return nil, err
	}
	return reg, nil
}

// Reload rebuilds the registry in place. Called from the admin
// CRUD endpoints after a write so UI edits take effect without
// restart. DB errors log + fall back to env; env errors are
// fatal at boot but reload preserves the previous state.
func Reload(ctx context.Context, reg *Registry, cfg *config.Config, dbSource DBSource, log *slog.Logger) error {
	envIntegration, envClient, err := buildEnvIntegration(cfg)
	if err != nil {
		return fmt.Errorf("vcs: env integration: %w", err)
	}

	var dbIntegrations []Integration
	var dbClient *ghscm.AppClient
	if dbSource != nil {
		rows, err := dbSource.ListBootstrapVCSIntegrations(ctx)
		if err != nil {
			log.Warn("vcs: DB list failed, keeping env-only", "err", err)
		} else {
			for _, row := range rows {
				client, meta, err := buildDBIntegration(row)
				if err != nil {
					log.Warn("vcs: db integration skipped",
						"name", row.Name, "kind", row.Kind, "err", err)
					continue
				}
				if client != nil && dbClient == nil && row.Kind == store.VCSKindGitHubApp {
					dbClient = client
					log.Info("vcs: active github_app from db", "name", row.Name)
				}
				dbIntegrations = append(dbIntegrations, meta)
			}
		}
	}

	// Merge in stable insertion order: env first (so its position
	// stays familiar in the UI), DB second. Name-collision: DB
	// metadata replaces env metadata at the env position.
	all := []Integration{}
	seen := map[string]int{}
	if envIntegration != nil {
		all = append(all, *envIntegration)
		seen[envIntegration.Name] = 0
	}
	for _, m := range dbIntegrations {
		if idx, dup := seen[m.Name]; dup {
			all[idx] = m
			continue
		}
		seen[m.Name] = len(all)
		all = append(all, m)
	}

	// Active GitHubApp client: DB wins over env when both present.
	active := envClient
	if dbClient != nil {
		active = dbClient
	}

	reg.Replace(active, all)
	log.Info("vcs: registry loaded",
		"env", envIntegration != nil, "db", len(dbIntegrations),
		"active_github_app", active != nil)
	return nil
}

// buildEnvIntegration returns the env-derived GitHub App (if the
// relevant env vars are set) as both an AppClient and its
// metadata shape. Returns (nil, nil, nil) when not configured —
// that's the normal case for dev deployments.
func buildEnvIntegration(cfg *config.Config) (*Integration, *ghscm.AppClient, error) {
	if cfg.GithubAppID == 0 {
		return nil, nil, nil
	}
	client, err := ghscm.NewAppClientFromEnv(
		cfg.GithubAppID,
		cfg.GithubAppPrivateKeyPEM,
		cfg.GithubAppPrivateKeyFile,
		cfg.GithubAppAPIBase,
	)
	if err != nil {
		return nil, nil, err
	}
	appID := cfg.GithubAppID
	meta := &Integration{
		Name:        "env",
		Kind:        store.VCSKindGitHubApp,
		DisplayName: "GitHub App (env)",
		AppID:       &appID,
		APIBase:     cfg.GithubAppAPIBase,
		Enabled:     true,
		Source:      SourceEnv,
	}
	return meta, client, nil
}

// buildDBIntegration instantiates a concrete client from a
// decrypted DB row and returns the metadata shape to surface in
// admin listings.
func buildDBIntegration(row store.BootstrapVCSIntegration) (*ghscm.AppClient, Integration, error) {
	meta := Integration{
		ID:          row.ID,
		Name:        row.Name,
		Kind:        row.Kind,
		DisplayName: row.DisplayName,
		AppID:       row.AppID,
		APIBase:     row.APIBase,
		Enabled:     row.Enabled,
		Source:      SourceDB,
		UpdatedAt:   row.UpdatedAt,
	}
	switch row.Kind {
	case store.VCSKindGitHubApp:
		if row.AppID == nil || *row.AppID <= 0 {
			return nil, meta, fmt.Errorf("github_app missing app_id")
		}
		if len(row.PrivateKeyPEM) == 0 {
			return nil, meta, fmt.Errorf("github_app missing private key")
		}
		client, err := ghscm.NewAppClient(ghscm.AppConfig{
			AppID:         *row.AppID,
			PrivateKeyPEM: row.PrivateKeyPEM,
			APIBase:       row.APIBase,
		})
		if err != nil {
			return nil, meta, fmt.Errorf("build app client: %w", err)
		}
		return client, meta, nil
	default:
		return nil, meta, fmt.Errorf("unsupported kind %q", row.Kind)
	}
}
