// Command gocdnext-agent connects to a server, pulls jobs, and runs them inside
// containers. Designed to run as a container in Kubernetes or as a binary on a VM.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/gocdnext/gocdnext/agent/internal/rpc"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("agent starting", "server", cfg.ServerAddr, "agent_id", cfg.AgentID)

	client := rpc.New(cfg, logger)
	if err := client.Run(ctx); err != nil {
		logger.Error("agent run", "err", err)
		os.Exit(1)
	}
	logger.Info("agent stopped")
}

func loadConfig() (rpc.Config, error) {
	addr := os.Getenv("GOCDNEXT_SERVER_ADDR")
	name := os.Getenv("GOCDNEXT_AGENT_NAME")
	token := os.Getenv("GOCDNEXT_AGENT_TOKEN")
	if addr == "" || name == "" || token == "" {
		return rpc.Config{}, fmt.Errorf(
			"GOCDNEXT_SERVER_ADDR, GOCDNEXT_AGENT_NAME and GOCDNEXT_AGENT_TOKEN are required")
	}

	var tags []string
	if raw := os.Getenv("GOCDNEXT_AGENT_TAGS"); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}

	var capacity int32 = 1
	if raw := os.Getenv("GOCDNEXT_AGENT_CAPACITY"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return rpc.Config{}, fmt.Errorf("GOCDNEXT_AGENT_CAPACITY must be a positive int, got %q", raw)
		}
		capacity = int32(n)
	}

	return rpc.Config{
		ServerAddr: addr,
		AgentID:    name,
		Token:      token,
		Version:    versionString(),
		Tags:       tags,
		Capacity:   capacity,
	}, nil
}

// versionString returns a static version string until we wire ldflags at build
// time. Keeping it here lets us bump once instead of hunting the literal.
func versionString() string { return "0.1.0-dev" }
