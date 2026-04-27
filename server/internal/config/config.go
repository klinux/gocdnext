// Package config loads server configuration from environment variables.
// Env beats file on purpose — 12-factor; no need for a config file for the MVP.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	HTTPAddr     string
	GRPCAddr     string
	DatabaseURL  string
	LogLevel     slog.Level
	SecretKeyHex string // 64-char hex AES-256 key for encrypting secrets at rest

	// Artifact storage. Backend selects the implementation; the other
	// fields are read only for the selected backend.
	ArtifactsBackend    string // "filesystem" (default), "s3", "gcs"
	ArtifactsFSRoot     string // filesystem: absolute path on the server
	ArtifactsPublicBase string // external base URL used to build signed URLs
	ArtifactsSignKeyHex string // hex HMAC key for filesystem signed URLs
	ArtifactsMaxBodyMB  int64  // PUT body cap in MiB; 0 disables

	// S3 config (used when ArtifactsBackend == "s3").
	ArtifactsS3Bucket       string
	ArtifactsS3Region       string
	ArtifactsS3Endpoint     string // optional; set for R2/Tigris/LocalStack
	ArtifactsS3AccessKey    string // optional; default cred chain if empty
	ArtifactsS3SecretKey    string
	ArtifactsS3UsePathStyle bool
	ArtifactsS3EnsureBucket bool // best-effort CreateBucket on startup

	// GCS config (used when ArtifactsBackend == "gcs").
	ArtifactsGCSBucket          string
	ArtifactsGCSCredentialsFile string // path to service-account JSON; enables SignedURL
	ArtifactsGCSCredentialsJSON string // inline JSON (same schema)
	ArtifactsGCSProjectID       string // required only for EnsureBucket
	ArtifactsGCSEnsureBucket    bool

	// Retention / sweeper knobs. 0 on any quota = disabled.
	ArtifactsKeepLast          int   // keep N most recent runs per pipeline; 0 disables
	ArtifactsProjectQuotaBytes int64 // per-project soft cap; 0 disables
	ArtifactsGlobalQuotaBytes  int64 // global hard cap; 0 disables

	// Cache eviction. Default is 30 days of inactivity; 0 disables.
	CacheTTLDays int
	// Cache per-project size cap. 0 disables — TTL-only eviction.
	CacheProjectQuotaBytes int64
	// Cache global size cap. 0 disables.
	CacheGlobalQuotaBytes int64

	// RunnerProfilesFile is an optional path to a YAML the server
	// reads on boot and upserts into the runner_profiles table. The
	// Helm chart points this at a ConfigMap-mounted file so operators
	// can pin a cluster's profile catalogue declaratively. Empty
	// disables seeding — the table is purely admin-UI driven.
	RunnerProfilesFile string

	// PluginCatalogDir is the root the server scans on boot for
	// `plugin.yaml` manifests — one per `<dir>/plugin.yaml`. Empty
	// disables catalog loading, which means pipelines using
	// `uses:` pass validation by default (third-party images).
	// Typical value: "./plugins" when running the server from the
	// monorepo, or a volume-mounted path in production images.
	PluginCatalogDir string

	// GitHub App (optional): enables auto-register webhook + Checks
	// API status reporting. AppID + (PrivateKey OR PrivateKeyFile)
	// must all be set to enable; empty = App disabled, webhooks
	// are created manually by ops and Checks API is skipped.
	GithubAppID             int64
	GithubAppPrivateKeyPEM  string // inline PEM content
	GithubAppPrivateKeyFile string // path to PEM file (alternative)
	GithubAppAPIBase        string // default https://api.github.com

	// PublicBase is the externally-reachable URL of this server.
	// Used when building webhook URLs we register at GitHub. For
	// local dev with ngrok, set to the ngrok HTTPS URL.
	PublicBase string

	// WebhookPublicURL optionally overrides PublicBase for the
	// URL we hand to GitHub when installing a repo webhook.
	// GitHub refuses to register hooks pointing at localhost
	// (422 "not reachable over the public Internet"), so local
	// dev can point this at a smee.io / ngrok tunnel while the
	// UI keeps serving from http://localhost:8153.
	//
	// Interpretation: full URL including path. Empty → fall back
	// to PublicBase + "/api/webhooks/github".
	WebhookPublicURL string

	// Secret backend: "db" (default, uses SecretKeyHex to decrypt
	// ciphertext stored in Postgres) or "kubernetes" (reads K8s
	// Secret objects named by template).
	SecretBackend      string
	SecretK8sNamespace string
	SecretK8sTemplate  string // default "gocdnext-secrets-{slug}"
	SecretK8sKubeconfig string // empty = in-cluster

	// Auth (UI.6): GOCDNEXT_AUTH_ENABLED=true turns on session
	// enforcement + /auth routes. When disabled (the default) the
	// API stays open so existing dev workflows keep working.
	//
	// PublicBase is reused as the callback base — we only mint
	// callback URLs when auth is on, so the startup check is local
	// to NewRegistryFromConfig.
	AuthEnabled       bool
	AuthAdminEmails   []string // comma list; matched case-insensitively on first login
	AuthAllowedDomains []string // optional allowlist; empty = anyone who passes IdP

	// Per-provider settings. Each provider becomes "enabled" by
	// having its CLIENT_ID set. Issuer defaults to the vendor's
	// well-known URL when left blank for Google.
	AuthGitHubClientID     string
	AuthGitHubClientSecret string
	AuthGitHubAPIBase      string // GitHub Enterprise override

	AuthGoogleClientID     string
	AuthGoogleClientSecret string
	AuthGoogleIssuer       string // default https://accounts.google.com

	AuthKeycloakClientID     string
	AuthKeycloakClientSecret string
	AuthKeycloakIssuer       string

	AuthOIDCClientID     string
	AuthOIDCClientSecret string
	AuthOIDCIssuer       string
	AuthOIDCDisplayName  string
}

func Load() (*Config, error) {
	maxBodyMB, err := strconv.ParseInt(env("GOCDNEXT_ARTIFACTS_MAX_BODY_MB", "2048"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GOCDNEXT_ARTIFACTS_MAX_BODY_MB: %w", err)
	}

	// GOCDNEXT_WEBHOOK_TOKEN used to be the global HMAC secret.
	// Per-repo secrets killed it in UI.10.a — flag as deprecated
	// if anyone still sets it so onboarding docs don't mislead.
	if legacy := env("GOCDNEXT_WEBHOOK_TOKEN", ""); legacy != "" {
		slog.Warn("config: GOCDNEXT_WEBHOOK_TOKEN is deprecated and ignored; secrets are per-repo now (see /settings/integrations)")
	}

	// GOCDNEXT_CONFIG_FOLDER was never read off the Config struct
	// and is now per-project (projects.config_path). Warn the
	// operator if they still have it in env so the onboarding
	// docs don't mislead.
	if legacy := env("GOCDNEXT_CONFIG_FOLDER", ""); legacy != "" {
		slog.Warn("config: GOCDNEXT_CONFIG_FOLDER is deprecated and ignored; set it per project via /projects → Connect repo")
	}

	c := &Config{
		HTTPAddr:     env("GOCDNEXT_HTTP_ADDR", ":8153"),
		GRPCAddr:     env("GOCDNEXT_GRPC_ADDR", ":8154"),
		DatabaseURL:  env("GOCDNEXT_DATABASE_URL", ""),
		SecretKeyHex: env("GOCDNEXT_SECRET_KEY", ""),

		ArtifactsBackend:    strings.ToLower(env("GOCDNEXT_ARTIFACTS_BACKEND", "filesystem")),
		ArtifactsFSRoot:     env("GOCDNEXT_ARTIFACTS_FS_ROOT", "/var/lib/gocdnext/artifacts"),
		ArtifactsPublicBase: env("GOCDNEXT_ARTIFACTS_PUBLIC_BASE", "http://localhost:8153"),
		ArtifactsSignKeyHex: env("GOCDNEXT_ARTIFACTS_SIGN_KEY", ""),
		ArtifactsMaxBodyMB:  maxBodyMB,

		ArtifactsS3Bucket:       env("GOCDNEXT_ARTIFACTS_S3_BUCKET", ""),
		ArtifactsS3Region:       env("GOCDNEXT_ARTIFACTS_S3_REGION", "us-east-1"),
		ArtifactsS3Endpoint:     env("GOCDNEXT_ARTIFACTS_S3_ENDPOINT", ""),
		ArtifactsS3AccessKey:    env("GOCDNEXT_ARTIFACTS_S3_ACCESS_KEY", ""),
		ArtifactsS3SecretKey:    env("GOCDNEXT_ARTIFACTS_S3_SECRET_KEY", ""),
		ArtifactsS3UsePathStyle: strings.EqualFold(env("GOCDNEXT_ARTIFACTS_S3_USE_PATH_STYLE", "false"), "true"),
		ArtifactsS3EnsureBucket: strings.EqualFold(env("GOCDNEXT_ARTIFACTS_S3_ENSURE_BUCKET", "false"), "true"),

		ArtifactsGCSBucket:          env("GOCDNEXT_ARTIFACTS_GCS_BUCKET", ""),
		ArtifactsGCSCredentialsFile: env("GOCDNEXT_ARTIFACTS_GCS_CREDENTIALS_FILE", ""),
		ArtifactsGCSCredentialsJSON: env("GOCDNEXT_ARTIFACTS_GCS_CREDENTIALS_JSON", ""),
		ArtifactsGCSProjectID:       env("GOCDNEXT_ARTIFACTS_GCS_PROJECT_ID", ""),
		ArtifactsGCSEnsureBucket:    strings.EqualFold(env("GOCDNEXT_ARTIFACTS_GCS_ENSURE_BUCKET", "false"), "true"),
	}

	keepLast, err := strconv.Atoi(env("GOCDNEXT_ARTIFACTS_KEEP_LAST", "30"))
	if err != nil {
		return nil, fmt.Errorf("GOCDNEXT_ARTIFACTS_KEEP_LAST: %w", err)
	}
	c.ArtifactsKeepLast = keepLast

	projectQuota, err := strconv.ParseInt(env("GOCDNEXT_ARTIFACTS_PROJECT_QUOTA_BYTES", "107374182400"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GOCDNEXT_ARTIFACTS_PROJECT_QUOTA_BYTES: %w", err)
	}
	c.ArtifactsProjectQuotaBytes = projectQuota

	globalQuota, err := strconv.ParseInt(env("GOCDNEXT_ARTIFACTS_GLOBAL_QUOTA_BYTES", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GOCDNEXT_ARTIFACTS_GLOBAL_QUOTA_BYTES: %w", err)
	}
	c.ArtifactsGlobalQuotaBytes = globalQuota

	cacheTTLDays, err := strconv.Atoi(env("GOCDNEXT_CACHE_TTL_DAYS", "30"))
	if err != nil {
		return nil, fmt.Errorf("GOCDNEXT_CACHE_TTL_DAYS: %w", err)
	}
	c.CacheTTLDays = cacheTTLDays

	cacheProjectQuota, err := strconv.ParseInt(env("GOCDNEXT_CACHE_PROJECT_QUOTA_BYTES", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GOCDNEXT_CACHE_PROJECT_QUOTA_BYTES: %w", err)
	}
	c.CacheProjectQuotaBytes = cacheProjectQuota

	cacheGlobalQuota, err := strconv.ParseInt(env("GOCDNEXT_CACHE_GLOBAL_QUOTA_BYTES", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GOCDNEXT_CACHE_GLOBAL_QUOTA_BYTES: %w", err)
	}
	c.CacheGlobalQuotaBytes = cacheGlobalQuota

	c.PluginCatalogDir = env("GOCDNEXT_PLUGIN_CATALOG_DIR", "")

	if raw := env("GOCDNEXT_GITHUB_APP_ID", ""); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("GOCDNEXT_GITHUB_APP_ID: %w", err)
		}
		c.GithubAppID = id
	}
	c.GithubAppPrivateKeyPEM = env("GOCDNEXT_GITHUB_APP_PRIVATE_KEY", "")
	c.GithubAppPrivateKeyFile = env("GOCDNEXT_GITHUB_APP_PRIVATE_KEY_FILE", "")
	c.GithubAppAPIBase = env("GOCDNEXT_GITHUB_APP_API_BASE", "")
	c.PublicBase = env("GOCDNEXT_PUBLIC_BASE", "")
	c.WebhookPublicURL = env("GOCDNEXT_WEBHOOK_PUBLIC_URL", "")
	c.RunnerProfilesFile = env("GOCDNEXT_RUNNER_PROFILES_FILE", "")
	c.SecretBackend = strings.ToLower(env("GOCDNEXT_SECRET_BACKEND", "db"))
	c.SecretK8sNamespace = env("GOCDNEXT_SECRET_K8S_NAMESPACE", "")
	c.SecretK8sTemplate = env("GOCDNEXT_SECRET_K8S_NAME_TEMPLATE", "")
	c.SecretK8sKubeconfig = env("GOCDNEXT_SECRET_K8S_KUBECONFIG", "")

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("GOCDNEXT_DATABASE_URL is required")
	}

	c.AuthEnabled = strings.EqualFold(env("GOCDNEXT_AUTH_ENABLED", "false"), "true")
	c.AuthAdminEmails = splitAndTrim(env("GOCDNEXT_AUTH_ADMIN_EMAILS", ""))
	c.AuthAllowedDomains = splitAndTrim(env("GOCDNEXT_AUTH_ALLOWED_DOMAINS", ""))
	c.AuthGitHubClientID = env("GOCDNEXT_AUTH_GITHUB_CLIENT_ID", "")
	c.AuthGitHubClientSecret = env("GOCDNEXT_AUTH_GITHUB_CLIENT_SECRET", "")
	c.AuthGitHubAPIBase = env("GOCDNEXT_AUTH_GITHUB_API_BASE", "")
	c.AuthGoogleClientID = env("GOCDNEXT_AUTH_GOOGLE_CLIENT_ID", "")
	c.AuthGoogleClientSecret = env("GOCDNEXT_AUTH_GOOGLE_CLIENT_SECRET", "")
	c.AuthGoogleIssuer = env("GOCDNEXT_AUTH_GOOGLE_ISSUER", "https://accounts.google.com")
	c.AuthKeycloakClientID = env("GOCDNEXT_AUTH_KEYCLOAK_CLIENT_ID", "")
	c.AuthKeycloakClientSecret = env("GOCDNEXT_AUTH_KEYCLOAK_CLIENT_SECRET", "")
	c.AuthKeycloakIssuer = env("GOCDNEXT_AUTH_KEYCLOAK_ISSUER", "")
	c.AuthOIDCClientID = env("GOCDNEXT_AUTH_OIDC_CLIENT_ID", "")
	c.AuthOIDCClientSecret = env("GOCDNEXT_AUTH_OIDC_CLIENT_SECRET", "")
	c.AuthOIDCIssuer = env("GOCDNEXT_AUTH_OIDC_ISSUER", "")
	c.AuthOIDCDisplayName = env("GOCDNEXT_AUTH_OIDC_NAME", "")

	switch strings.ToLower(env("GOCDNEXT_LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		c.LogLevel = slog.LevelInfo
	}

	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// splitAndTrim parses a comma-separated env var into a clean slice.
// Empty entries are dropped so "a,,b, " yields ["a","b"].
func splitAndTrim(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
