// Command gocdnext-server is the control-plane process: HTTP API, gRPC agent endpoint,
// webhook receiver, scheduler. See docs/architecture.md for the big picture.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	projectsapi "github.com/gocdnext/gocdnext/server/internal/api/projects"
	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dbCancel()
	pool, err := pgxpool.New(dbCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("pgxpool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(dbCtx); err != nil {
		logger.Error("postgres ping", "err", err)
		os.Exit(1)
	}

	st := store.New(pool)
	webhookHandler := webhook.NewHandler(cfg.WebhookToken, st, logger)
	projectsHandler := projectsapi.NewHandler(st, logger)

	sessions := grpcsrv.NewSessionStore()
	agentService := grpcsrv.NewAgentService(st, sessions, logger, 30)
	sched := scheduler.New(st, sessions, logger, cfg.DatabaseURL)

	grpcServer := grpc.NewServer()
	gocdnextv1.RegisterAgentServiceServer(grpcServer, agentService)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "gocdnext-server dev")
	})

	r.Post("/api/webhooks/github", webhookHandler.HandleGitHub)
	r.Post("/api/v1/projects/apply", projectsHandler.Apply)

	// TODO(phase-1): wire run/agent handlers.

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		logger.Error("grpc listen", "addr", cfg.GRPCAddr, "err", err)
		os.Exit(1)
	}
	go func() {
		logger.Info("grpc listening", "addr", cfg.GRPCAddr)
		if err := grpcServer.Serve(grpcLis); err != nil {
			logger.Error("grpc server", "err", err)
			os.Exit(1)
		}
	}()

	go func() {
		if err := sched.Run(ctx); err != nil {
			logger.Error("scheduler exited", "err", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	grpcServer.GracefulStop()
}
