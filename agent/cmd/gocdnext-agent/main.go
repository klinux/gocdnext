// Command gocdnext-agent connects to a server, pulls jobs, and runs them inside
// containers. Designed to run as a container in Kubernetes or as a binary on a VM.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	serverAddr := os.Getenv("GOCDNEXT_SERVER_ADDR")
	token := os.Getenv("GOCDNEXT_AGENT_TOKEN")
	if serverAddr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "GOCDNEXT_SERVER_ADDR and GOCDNEXT_AGENT_TOKEN are required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("agent starting", "server", serverAddr)

	// TODO(phase-1): establish gRPC stream, register, receive JobAssignment,
	// run container tasks via containerd/docker, stream logs back.
	<-ctx.Done()
	logger.Info("agent stopping")
}
