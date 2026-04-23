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
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/api/account"
	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	dashboardapi "github.com/gocdnext/gocdnext/server/internal/api/dashboard"
	pipelinesapi "github.com/gocdnext/gocdnext/server/internal/api/pipelines"
	projectsapi "github.com/gocdnext/gocdnext/server/internal/api/projects"
	runsapi "github.com/gocdnext/gocdnext/server/internal/api/runs"
	"github.com/gocdnext/gocdnext/server/internal/auth"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/checks"
	"github.com/gocdnext/gocdnext/server/internal/config"
	"github.com/gocdnext/gocdnext/server/internal/configsync"
	cronpkg "github.com/gocdnext/gocdnext/server/internal/cron"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
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
	switch cfg.SecretBackend {
	case "kubernetes":
		if cfg.SecretK8sNamespace == "" {
			logger.Error("secrets: GOCDNEXT_SECRET_K8S_NAMESPACE required when backend=kubernetes")
			os.Exit(1)
		}
		r, err := secrets.NewKubernetesResolver(secrets.KubernetesResolverConfig{
			Store:          st,
			Namespace:      cfg.SecretK8sNamespace,
			KubeconfigPath: cfg.SecretK8sKubeconfig,
			NameTemplate:   cfg.SecretK8sTemplate,
		})
		if err != nil {
			logger.Error("secrets resolver", "err", err)
			os.Exit(1)
		}
		resolver = r
		logger.Info("secrets subsystem enabled",
			"backend", "kubernetes",
			"namespace", cfg.SecretK8sNamespace)
	case "db", "":
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
	default:
		logger.Error("secrets: unknown backend", "backend", cfg.SecretBackend)
		os.Exit(1)
	}

	artifactStore, artifactHandler, err := buildArtifactBackend(cfg, logger)
	if err != nil {
		logger.Error("artifacts: init", "err", err)
		os.Exit(1)
	}

	// VCS registry — holds the currently active GitHub App client.
	// Populated from env at boot; the /settings/integrations admin
	// CRUD (UI.9.c) will Replace() the contents when the operator
	// saves new credentials, so checks.Reporter + auto-register see
	// the new App on the next call without a restart.
	//
	// st isn't wired in yet as a DBSource — the store cipher is
	// configured later in this file — so we bootstrap env-only now
	// and re-Reload() right after the cipher is set.
	vcsRegistry, err := vcs.BuildRegistry(context.Background(), cfg, nil, logger)
	if err != nil {
		logger.Error("vcs: bootstrap", "err", err)
		os.Exit(1)
	}
	if vcsRegistry.GitHubApp() != nil {
		logger.Info("github app client ready (env)", "app_id", cfg.GithubAppID)
	} else {
		logger.Info("github app not configured; auto-register webhook + Checks API disabled")
	}

	checksReporter := checks.NewReporter(st, vcsRegistry, cfg.PublicBase, logger)
	if checksReporter != nil {
		logger.Info("github checks reporter enabled")
	}

	// Single shared fetcher: same HTTP client + API base feed both
	// the webhook drift path (push → re-apply) and the project-apply
	// initial-sync path (bind → pull pipelines from HEAD). Reusing
	// one instance means connection-pool churn stays low when both
	// paths fire close together.
	gitHubFetcher := &configsync.GitHubFetcher{}

	webhookHandler := webhook.NewHandler(st, logger).
		WithConfigFetcher(gitHubFetcher).
		WithChecksReporter(checksReporter)
	projectsHandler := projectsapi.NewHandler(st, logger).
		WithCipher(cipher).
		WithConfigFetcher(gitHubFetcher)
	// Auto-register installs a repo webhook at apply time when
	// the project binds an scm_source. Requires PublicBase so
	// the hook URL GitHub pings back is reachable — without it
	// we skip wiring entirely and the admin UI reports the
	// feature off.
	if cfg.PublicBase != "" {
		projectsHandler = projectsHandler.WithAutoRegister(projectsapi.AutoRegisterConfig{
			VCS:              vcsRegistry,
			PublicBase:       cfg.PublicBase,
			WebhookPublicURL: cfg.WebhookPublicURL,
		})
	}
	runsHandler := runsapi.NewHandler(st, logger).
		WithConfigFetcher(gitHubFetcher)
	if artifactStore != nil {
		runsHandler = runsHandler.WithArtifactStore(artifactStore)
	}
	dashboardHandler := dashboardapi.NewHandler(st, logger)
	accountHandler := account.New(st, logger)
	pipelinesHandler := pipelinesapi.NewHandler(st, logger)

	sessions := grpcsrv.NewSessionStore()
	// Wire the session registry into the Cancel endpoint so
	// canceling a run actually pushes CancelJob frames to the
	// agents running jobs — otherwise cancel is DB-only and the
	// container keeps burning until it finishes naturally.
	runsHandler = runsHandler.WithCancelDispatcher(sessions)
	agentService := grpcsrv.NewAgentService(st, sessions, logger, 30).
		WithChecksReporter(checksReporter)
	if artifactStore != nil {
		agentService = agentService.WithArtifactStore(artifactStore, 15*time.Minute, 30*time.Minute, 30*24*time.Hour)
	}
	sched := scheduler.New(st, sessions, logger, cfg.DatabaseURL).WithSecretResolver(resolver)
	if artifactStore != nil {
		sched = sched.WithArtifactStore(artifactStore, 30*time.Minute)
	}
	reaper := scheduler.NewReaper(st, logger)
	sweeper := retention.New(st, artifactStore, logger).
		WithKeepLast(cfg.ArtifactsKeepLast).
		WithProjectQuotaBytes(cfg.ArtifactsProjectQuotaBytes).
		WithGlobalQuotaBytes(cfg.ArtifactsGlobalQuotaBytes)

	// WiringState carries the env-derived wiring only. The
	// dynamic bits (GitHub App active, auto-register effective)
	// are recomputed on each request via the vcs.Registry the
	// handler holds — otherwise the summary freezes at boot and
	// an admin who just saved a VCS integration sees a stale
	// "off" on the next page load.
	adminHandler := adminapi.NewHandler(st, sweeper, vcsRegistry, adminapi.WiringState{
		PublicBaseSet:    cfg.PublicBase != "",
		ChecksReporterOn: checksReporter != nil,
	}, logger)
	// authProvidersHandler + vcsIntegrationsHandler are wired
	// later (after the cipher + auth registry are ready). Declared
	// here so the router block can reference them.
	var authProvidersHandler *adminapi.AuthProvidersHandler
	var vcsIntegrationsHandler *adminapi.VCSIntegrationsHandler
	globalSecretsHandler := adminapi.NewGlobalSecretsHandler(st, cipher, logger)

	// DB-backed providers need the same AES cipher used for /secrets.
	// Wire it here so the admin UI can create/edit provider rows and
	// the bootstrap path can decrypt them. Without a cipher the
	// store layer returns ErrAuthProviderCipherUnset and only env-
	// based providers load.
	if cipher != nil {
		st.SetAuthCipher(cipher)
	}

	authCtx, authCancel := context.WithTimeout(context.Background(), 15*time.Second)
	authRegistry, err := auth.BuildRegistry(authCtx, cfg, st, logger)
	if err != nil {
		authCancel()
		logger.Error("auth: build registry", "err", err)
		os.Exit(1)
	}
	// Re-Reload the VCS registry now that the cipher is wired so
	// DB integrations are picked up on boot. Env-only load at the
	// top stays correct for deployments that haven't written any
	// DB rows yet.
	if cipher != nil {
		if err := vcs.Reload(authCtx, vcsRegistry, cfg, st, logger); err != nil {
			logger.Warn("vcs: db reload on boot", "err", err)
		}
	}
	authCancel()
	authProvidersHandler = adminapi.NewAuthProvidersHandler(st, authRegistry, cfg, logger)
	vcsIntegrationsHandler = adminapi.NewVCSIntegrationsHandler(st, vcsRegistry, cfg, logger)
	authMiddleware := authapi.NewMiddleware(st, logger, cfg.AuthEnabled)
	authHandler := authapi.NewHandler(authapi.Config{
		Registry:       authRegistry,
		Store:          st,
		Logger:         logger,
		PublicBase:     cfg.PublicBase,
		AllowedDomains: cfg.AuthAllowedDomains,
		AdminEmails:    cfg.AuthAdminEmails,
		DevMode:        !strings.HasPrefix(cfg.PublicBase, "https://"),
	})
	if cfg.AuthEnabled {
		logger.Info("auth: middleware enforcing sessions",
			"providers", authRegistry.Len(),
			"admin_emails", len(cfg.AuthAdminEmails),
			"allowed_domains", len(cfg.AuthAllowedDomains),
		)
	} else {
		logger.Warn("auth: DISABLED — API is open; set GOCDNEXT_AUTH_ENABLED=true in prod")
	}

	grpcServer := grpc.NewServer()
	gocdnextv1.RegisterAgentServiceServer(grpcServer, agentService)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(devCORS)
	r.Use(authMiddleware.LoadSession)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "gocdnext-server dev")
	})

	// /auth/* is public (login, callback, logout, providers).
	// /api/v1/me also mounts here; RequireAuth inside authHandler
	// decides on its own whether to 401.
	authHandler.Mount(r)

	if artifactHandler != nil {
		artifactHandler.Mount(r)
	}

	// /api/webhooks stays outside the auth middleware's enforcement
	// path — it authenticates via HMAC signature, not sessions.
	r.Post("/api/webhooks/github", webhookHandler.HandleGitHub)

	// Protected API surface: every mutation + every read that could
	// leak internal state sits inside this group. RequireAuth is a
	// pass-through when auth is globally disabled, so dev workflows
	// without GOCDNEXT_AUTH_ENABLED keep working.
	r.Group(func(p chi.Router) {
		p.Use(authMiddleware.RequireAuth)

		p.Post("/api/v1/projects/apply", projectsHandler.Apply)
		p.Post("/api/v1/projects/{slug}/sync", projectsHandler.Sync)
		p.Get("/api/v1/projects", projectsHandler.List)
		p.Get("/api/v1/projects/{slug}", projectsHandler.Detail)
		p.Delete("/api/v1/projects/{slug}", projectsHandler.Delete)
		p.Get("/api/v1/projects/{slug}/vsm", projectsHandler.VSM)
		p.Post("/api/v1/projects/{slug}/scm-sources/rotate-webhook-secret", projectsHandler.RotateWebhookSecret)
		p.Post("/api/v1/projects/{slug}/secrets", projectsHandler.SetSecret)
		p.Get("/api/v1/projects/{slug}/secrets", projectsHandler.ListSecrets)
		p.Delete("/api/v1/projects/{slug}/secrets/{name}", projectsHandler.DeleteSecret)
		p.Get("/api/v1/runs/{id}", runsHandler.Detail)
		p.Get("/api/v1/runs/{id}/artifacts", runsHandler.Artifacts)
		p.Post("/api/v1/runs/{id}/cancel", runsHandler.Cancel)
		p.Post("/api/v1/runs/{id}/rerun", runsHandler.Rerun)
		p.Post("/api/v1/job_runs/{id}/rerun", runsHandler.RerunJob)
		p.Post("/api/v1/pipelines/{id}/trigger", runsHandler.TriggerPipeline)
		p.Get("/api/v1/pipelines/{id}/yaml", pipelinesHandler.YAML)
		p.Get("/api/v1/dashboard/metrics", dashboardHandler.Metrics)
		p.Get("/api/v1/runs", dashboardHandler.RunsGlobal)
		p.Get("/api/v1/agents", dashboardHandler.Agents)
		p.Get("/api/v1/agents/{id}", dashboardHandler.AgentDetail)
		p.Get("/api/v1/account/preferences", accountHandler.GetPreferences)
		p.Put("/api/v1/account/preferences", accountHandler.PutPreferences)
	})

	// Admin API is gated on role=admin on top of RequireAuth. Users
	// and viewers get 403 even when authenticated.
	r.Group(func(p chi.Router) {
		p.Use(authMiddleware.RequireAuth)
		p.Use(authMiddleware.RequireRole("admin"))

		p.Get("/api/v1/admin/retention", adminHandler.Retention)
		p.Get("/api/v1/admin/webhooks", adminHandler.Webhooks)
		p.Get("/api/v1/admin/webhooks/{id}", adminHandler.WebhookDetail)
		p.Get("/api/v1/admin/health", adminHandler.Health)
		p.Get("/api/v1/admin/integrations/github", adminHandler.IntegrationGitHub)
		p.Get("/api/v1/admin/auth/providers", authProvidersHandler.List)
		p.Post("/api/v1/admin/auth/providers", authProvidersHandler.Upsert)
		p.Delete("/api/v1/admin/auth/providers/{id}", authProvidersHandler.Delete)
		p.Post("/api/v1/admin/auth/providers/reload", authProvidersHandler.Reload)
		p.Get("/api/v1/admin/integrations/vcs", vcsIntegrationsHandler.List)
		p.Post("/api/v1/admin/integrations/vcs", vcsIntegrationsHandler.Upsert)
		p.Delete("/api/v1/admin/integrations/vcs/{id}", vcsIntegrationsHandler.Delete)
		p.Post("/api/v1/admin/integrations/vcs/reload", vcsIntegrationsHandler.Reload)
		p.Get("/api/v1/admin/secrets", globalSecretsHandler.List)
		p.Post("/api/v1/admin/secrets", globalSecretsHandler.Set)
		p.Delete("/api/v1/admin/secrets/{name}", globalSecretsHandler.Delete)
	})

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

	cronTicker := cronpkg.New(st, logger)
	go func() {
		if err := cronTicker.Run(ctx); err != nil {
			logger.Error("cron ticker exited", "err", err)
		}
	}()

	go func() {
		if err := sweeper.Run(ctx); err != nil {
			logger.Error("artifact sweeper exited", "err", err)
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
		// Credentials=true forces us off "*" on Origin; echo the
		// requester's Origin so the browser lets the cookie flow
		// both ways. Dev-only; production should front this with a
		// reverse proxy on the same origin.
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-GitHub-Event, X-GitHub-Delivery, X-Hub-Signature-256")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
