// Package config loads server configuration from environment variables.
// Env beats file on purpose — 12-factor; no need for a config file for the MVP.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	HTTPAddr     string
	GRPCAddr     string
	DatabaseURL  string
	LogLevel     slog.Level
	WebhookToken string // fallback shared secret until per-material secrets ship
	ArtifactsURL string // s3://bucket or file:///var/lib/gocdnext/artifacts
}

func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:     env("GOCDNEXT_HTTP_ADDR", ":8153"),
		GRPCAddr:     env("GOCDNEXT_GRPC_ADDR", ":8154"),
		DatabaseURL:  env("GOCDNEXT_DATABASE_URL", ""),
		WebhookToken: env("GOCDNEXT_WEBHOOK_TOKEN", ""),
		ArtifactsURL: env("GOCDNEXT_ARTIFACTS_URL", "file:///var/lib/gocdnext/artifacts"),
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
