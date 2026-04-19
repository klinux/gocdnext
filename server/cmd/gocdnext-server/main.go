// Command gocdnext-server is the control-plane process: HTTP API, gRPC agent endpoint,
// webhook receiver, scheduler. See docs/architecture.md for the big picture.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	runsapi "github.com/gocdnext/gocdnext/server/internal/api/runs"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
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

	var cipher *crypto.Cipher
	var resolver secrets.Resolver = secrets.NopResolver{}
	if cfg.SecretKeyHex != "" {
		c, err := crypto.NewCipherFromHex(cfg.SecretKeyHex)
		if err != nil {
			logger.Error("secret key", "err", err)
			os.Exit(1)
		}
		cipher = c
		r, err := secrets.NewDBResolver(st, cipher)
		if err != nil {
			logger.Error("secrets resolver", "err", err)
			os.Exit(1)
		}
		resolver = r
		logger.Info("secrets subsystem enabled", "backend", "db")
	} else {
		logger.Warn("GOCDNEXT_SECRET_KEY not set; /secrets endpoints will return 503 and jobs that declare secrets will fail at dispatch")
	}

	artifactStore, artifactHandler, err := buildArtifactBackend(cfg, logger)
	if err != nil {
		logger.Error("artifacts: init", "err", err)
		os.Exit(1)
	}

	webhookHandler := webhook.NewHandler(cfg.WebhookToken, st, logger).
		WithConfigFetcher(&webhook.GitHubConfigFetcher{})
	projectsHandler := projectsapi.NewHandler(st, logger).WithCipher(cipher)
	runsHandler := runsapi.NewHandler(st, logger)

	sessions := grpcsrv.NewSessionStore()
	agentService := grpcsrv.NewAgentService(st, sessions, logger, 30)
	if artifactStore != nil {
		agentService = agentService.WithArtifactStore(artifactStore, 15*time.Minute, 30*24*time.Hour)
	}
	sched := scheduler.New(st, sessions, logger, cfg.DatabaseURL).WithSecretResolver(resolver)
	reaper := scheduler.NewReaper(st, logger)

	grpcServer := grpc.NewServer()
	gocdnextv1.RegisterAgentServiceServer(grpcServer, agentService)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(devCORS)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "gocdnext-server dev")
	})

	if artifactHandler != nil {
		artifactHandler.Mount(r)
	}
	r.Post("/api/webhooks/github", webhookHandler.HandleGitHub)
	r.Post("/api/v1/projects/apply", projectsHandler.Apply)
	r.Get("/api/v1/projects", projectsHandler.List)
	r.Get("/api/v1/projects/{slug}", projectsHandler.Detail)
	r.Post("/api/v1/projects/{slug}/secrets", projectsHandler.SetSecret)
	r.Get("/api/v1/projects/{slug}/secrets", projectsHandler.ListSecrets)
	r.Delete("/api/v1/projects/{slug}/secrets/{name}", projectsHandler.DeleteSecret)
	r.Get("/api/v1/runs/{id}", runsHandler.Detail)

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

	go func() {
		if err := reaper.Run(ctx); err != nil {
			logger.Error("reaper exited", "err", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	grpcServer.GracefulStop()
}

// buildArtifactBackend wires the configured artefact Store. Backends:
// filesystem (default), s3, gcs. The HTTP handler returned is non-nil
// only for filesystem — cloud backends serve signed URLs from their own
// endpoints and don't need routes on our server.
func buildArtifactBackend(cfg *config.Config, logger *slog.Logger) (artifacts.Store, *artifacts.Handler, error) {
	switch cfg.ArtifactsBackend {
	case "filesystem", "":
		signKey, err := resolveArtifactSignKey(cfg, logger)
		if err != nil {
			return nil, nil, err
		}
		signer, err := artifacts.NewSigner(signKey)
		if err != nil {
			return nil, nil, fmt.Errorf("signer: %w", err)
		}
		fs, err := artifacts.NewFilesystemStore(cfg.ArtifactsFSRoot, cfg.ArtifactsPublicBase, signer)
		if err != nil {
			return nil, nil, fmt.Errorf("filesystem store: %w", err)
		}
		maxBody := cfg.ArtifactsMaxBodyMB * 1024 * 1024
		h := artifacts.NewHandler(fs, logger, maxBody)
		logger.Info("artifacts backend: filesystem",
			"root", cfg.ArtifactsFSRoot,
			"public_base", cfg.ArtifactsPublicBase,
			"max_body_mb", cfg.ArtifactsMaxBodyMB,
		)
		return fs, h, nil
	case "s3":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s3Store, err := artifacts.NewS3Store(ctx, artifacts.S3Config{
			Bucket:       cfg.ArtifactsS3Bucket,
			Region:       cfg.ArtifactsS3Region,
			Endpoint:     cfg.ArtifactsS3Endpoint,
			AccessKey:    cfg.ArtifactsS3AccessKey,
			SecretKey:    cfg.ArtifactsS3SecretKey,
			UsePathStyle: cfg.ArtifactsS3UsePathStyle,
		})
		if err != nil {
			return nil, nil, err
		}
		if cfg.ArtifactsS3EnsureBucket {
			if err := s3Store.EnsureBucket(ctx, cfg.ArtifactsS3Region); err != nil {
				return nil, nil, fmt.Errorf("ensure s3 bucket: %w", err)
			}
		}
		logger.Info("artifacts backend: s3",
			"bucket", cfg.ArtifactsS3Bucket,
			"region", cfg.ArtifactsS3Region,
			"endpoint", cfg.ArtifactsS3Endpoint,
			"path_style", cfg.ArtifactsS3UsePathStyle,
		)
		return s3Store, nil, nil
	case "gcs":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		gcsStore, err := artifacts.NewGCSStore(ctx, artifacts.GCSConfig{
			Bucket:          cfg.ArtifactsGCSBucket,
			CredentialsFile: cfg.ArtifactsGCSCredentialsFile,
			CredentialsJSON: []byte(cfg.ArtifactsGCSCredentialsJSON),
		})
		if err != nil {
			return nil, nil, err
		}
		if cfg.ArtifactsGCSEnsureBucket {
			if cfg.ArtifactsGCSProjectID == "" {
				return nil, nil, fmt.Errorf("ensure gcs bucket: GOCDNEXT_ARTIFACTS_GCS_PROJECT_ID required")
			}
			if err := gcsStore.EnsureBucket(ctx, cfg.ArtifactsGCSProjectID); err != nil {
				return nil, nil, fmt.Errorf("ensure gcs bucket: %w", err)
			}
		}
		logger.Info("artifacts backend: gcs",
			"bucket", cfg.ArtifactsGCSBucket,
			"creds_file", cfg.ArtifactsGCSCredentialsFile != "",
			"creds_inline", cfg.ArtifactsGCSCredentialsJSON != "",
		)
		return gcsStore, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported artifacts backend %q (expected: filesystem, s3, gcs)", cfg.ArtifactsBackend)
	}
}

// resolveArtifactSignKey decodes the hex signing key from config; if
// unset, generates a random 32-byte key and warns loudly. Regenerating
// on restart invalidates in-flight signed URLs — acceptable for dev, not
// for prod (operator should set the env var).
func resolveArtifactSignKey(cfg *config.Config, logger *slog.Logger) ([]byte, error) {
	if cfg.ArtifactsSignKeyHex != "" {
		b, err := hex.DecodeString(cfg.ArtifactsSignKeyHex)
		if err != nil {
			return nil, fmt.Errorf("GOCDNEXT_ARTIFACTS_SIGN_KEY: decode: %w", err)
		}
		if len(b) < 16 {
			return nil, fmt.Errorf("GOCDNEXT_ARTIFACTS_SIGN_KEY: need >= 16 bytes (32 hex chars)")
		}
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("ephemeral sign key: %w", err)
	}
	logger.Warn("GOCDNEXT_ARTIFACTS_SIGN_KEY not set; generated ephemeral key (signed URLs will break across restart)")
	return b, nil
}

// devCORS opens the HTTP API to any origin so the Next.js dev server (port
// 3000) can call the control plane (port 8153) during development. Replace
// with a scoped policy (or a reverse proxy in front) before exposing this
// server outside a private network.
func devCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-GitHub-Event, X-GitHub-Delivery, X-Hub-Signature-256")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
