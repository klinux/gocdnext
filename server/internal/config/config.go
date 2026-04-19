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
	WebhookToken string // fallback shared secret until per-material secrets ship
	ConfigFolder string // folder name in repos holding pipeline YAMLs (.gocdnext)
	SecretKeyHex string // 64-char hex AES-256 key for encrypting secrets at rest

	// Artifact storage. Backend selects the implementation; the other
	// fields are read only for the selected backend.
	ArtifactsBackend    string // "filesystem" (default), "s3", "gcs"
	ArtifactsFSRoot     string // filesystem: absolute path on the server
	ArtifactsPublicBase string // external base URL used to build signed URLs
	ArtifactsSignKeyHex string // hex HMAC key for filesystem signed URLs
	ArtifactsMaxBodyMB  int64  // PUT body cap in MiB; 0 disables
}

func Load() (*Config, error) {
	maxBodyMB, err := strconv.ParseInt(env("GOCDNEXT_ARTIFACTS_MAX_BODY_MB", "2048"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GOCDNEXT_ARTIFACTS_MAX_BODY_MB: %w", err)
	}

	c := &Config{
		HTTPAddr:     env("GOCDNEXT_HTTP_ADDR", ":8153"),
		GRPCAddr:     env("GOCDNEXT_GRPC_ADDR", ":8154"),
		DatabaseURL:  env("GOCDNEXT_DATABASE_URL", ""),
		WebhookToken: env("GOCDNEXT_WEBHOOK_TOKEN", ""),
		ConfigFolder: env("GOCDNEXT_CONFIG_FOLDER", ".gocdnext"),
		SecretKeyHex: env("GOCDNEXT_SECRET_KEY", ""),

		ArtifactsBackend:    strings.ToLower(env("GOCDNEXT_ARTIFACTS_BACKEND", "filesystem")),
		ArtifactsFSRoot:     env("GOCDNEXT_ARTIFACTS_FS_ROOT", "/var/lib/gocdnext/artifacts"),
		ArtifactsPublicBase: env("GOCDNEXT_ARTIFACTS_PUBLIC_BASE", "http://localhost:8153"),
		ArtifactsSignKeyHex: env("GOCDNEXT_ARTIFACTS_SIGN_KEY", ""),
		ArtifactsMaxBodyMB:  maxBodyMB,
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("GOCDNEXT_DATABASE_URL is required")
	}

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
