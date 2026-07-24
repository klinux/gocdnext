// Command gocdnext-agent connects to a server, pulls jobs, and runs them inside
// containers. Designed to run as a container in Kubernetes or as a binary on a VM.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/metrics"
	"github.com/gocdnext/gocdnext/agent/internal/rpc"
	"github.com/gocdnext/gocdnext/agent/internal/runner"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	// Subcommand dispatch. Kept hand-rolled (no cobra) because the
	// surface is small: version + prep. Anything else falls through
	// to the historical "no subcommand" agent main loop.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(Version)
			return
		case "prep":
			// Init container entrypoint: deserialise the JobAssignment
			// mounted into the pod, run workspace materialisation
			// (clone + artifact download), exit. K8s gates the main
			// containers on this exit code — non-zero = pod fails.
			if err := runPrep(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "prep:", err)
				os.Exit(1)
			}
			return
		}
	}
	// Defence in depth: a flag like `--version=true` would slip past
	// the os.Args[1] switch above. Keep the historical loop so the
	// long-form keeps working.
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-v" || a == "version" {
			fmt.Println(Version)
			return
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	eng, err := buildEngine(logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg.Engine = eng

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.MetricsAddr != "" {
		serveMetrics(ctx, cfg.MetricsAddr, logger)
	}

	logger.Info("agent starting", "server", cfg.ServerAddr, "agent_id", cfg.AgentID)

	client := rpc.New(cfg, logger)
	if err := client.Run(ctx); err != nil {
		logger.Error("agent run", "err", err)
		os.Exit(1)
	}
	logger.Info("agent stopped")
}

// serveMetrics starts the Prometheus /metrics listener in a goroutine and shuts
// it down when ctx is canceled. A bind failure is logged and swallowed — a
// metrics-port clash must never take down a worker, and the agent stays useful
// without the endpoint.
func serveMetrics(ctx context.Context, addr string, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 15 * time.Second}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	go func() {
		logger.Info("agent metrics listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("agent metrics listener stopped", "err", err)
		}
	}()
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

	workspaceRoot, err := resolveWorkspaceRoot()
	if err != nil {
		return rpc.Config{}, err
	}

	return rpc.Config{
		ServerAddr:    addr,
		AgentID:       name,
		Token:         token,
		Version:       versionString(),
		Tags:          tags,
		Capacity:      capacity,
		WorkspaceRoot: workspaceRoot,
		MetricsAddr:   metricsAddr(),
	}, nil
}

// metricsAddr resolves the /metrics listen address with three distinct states,
// which is why it uses LookupEnv (not Getenv): env UNSET → a loopback default
// (a bare-metal/VM agent can curl it, never the network); env set to EMPTY →
// disabled (the chart sets this explicitly when metrics.enabled=false, so the
// default cannot silently leave a listener running in a pod); env set to a
// VALUE → that address.
func metricsAddr() string {
	if v, ok := os.LookupEnv("GOCDNEXT_METRICS_ADDR"); ok {
		return v
	}
	return "127.0.0.1:9464"
}

// resolveWorkspaceRoot derives the path the runner clones into so
// the agent + every spawned job pod see the SAME bytes:
//
//  1. GOCDNEXT_WORKSPACE_ROOT explicit override → use as-is. Reserved
//     for operators who mount the PVC at a non-default path or want
//     /tmp behaviour for a particular reason.
//  2. engine == "kubernetes" + workspace mode == "shared" →
//     GOCDNEXT_K8S_WORKSPACE_PATH (the PVC mount point job pods
//     receive). REQUIRED in this mode; an empty value is a misconfig
//     the operator should hear about at boot, not as a `lstat`
//     failure inside a buildx plugin three minutes into the first
//     real job.
//  3. engine == "kubernetes" + workspace mode == "isolated" → empty;
//     the agent never writes to a workspace dir in isolated mode
//     (prep runs inside each job pod's init container against the
//     pod's own ephemeral PVC). Runner falls back to /tmp default for
//     bookkeeping only.
//  4. shell / docker / unset → empty, runner falls back to its
//     `/tmp/gocdnext-workspace/` default. Shell tasks run on the
//     agent's own fs; docker tasks share the host fs through the
//     docker socket. Either way `/tmp` is fine.
//
// The chart no longer needs to set GOCDNEXT_WORKSPACE_ROOT — picking
// the k8s engine + setting the PVC mount path is enough.
func resolveWorkspaceRoot() (string, error) {
	if override := strings.TrimSpace(os.Getenv("GOCDNEXT_WORKSPACE_ROOT")); override != "" {
		return override, nil
	}
	engine := strings.ToLower(strings.TrimSpace(os.Getenv("GOCDNEXT_AGENT_ENGINE")))
	if engine != "kubernetes" {
		return "", nil
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("GOCDNEXT_K8S_WORKSPACE_MODE")))
	if mode == "isolated" {
		// Isolated mode: agent doesn't materialise the workspace
		// itself; init container does it inside each job pod. No
		// path requirement; fall through to /tmp default.
		return "", nil
	}
	mount := strings.TrimSpace(os.Getenv("GOCDNEXT_K8S_WORKSPACE_PATH"))
	if mount == "" {
		return "", fmt.Errorf(
			"GOCDNEXT_AGENT_ENGINE=kubernetes with GOCDNEXT_K8S_WORKSPACE_MODE=shared " +
				"requires GOCDNEXT_K8S_WORKSPACE_PATH to be set so the agent + spawned " +
				"job pods share the same PVC view; the chart wires it to " +
				"agent.workspace.mountPath")
	}
	return mount, nil
}

// buildEngine picks the runtime used for each script task. The
// default (unset / "shell") keeps the historical behaviour:
// `sh -c $script` on the agent host. Set GOCDNEXT_AGENT_ENGINE=
// kubernetes to spawn a Pod per task inside the configured cluster.
func buildEngine(logger *slog.Logger) (engine.Engine, error) {
	choice := strings.ToLower(os.Getenv("GOCDNEXT_AGENT_ENGINE"))
	switch choice {
	case "", "shell":
		logger.Info("agent engine: shell")
		return engine.NewShell(), nil
	case "docker":
		cfg := engine.DockerConfig{
			SocketPath:   os.Getenv("GOCDNEXT_DOCKER_SOCKET"),
			DefaultImage: os.Getenv("GOCDNEXT_DOCKER_DEFAULT_IMAGE"),
			PullPolicy:   os.Getenv("GOCDNEXT_DOCKER_PULL_POLICY"),
		}
		if raw := os.Getenv("GOCDNEXT_DOCKER_EXTRA_ARGS"); raw != "" {
			// Naive split on spaces — doesn't handle quoted values,
			// but the extra-args knob is rare enough that shellwords
			// would be overkill. Upgrade if someone files an issue.
			for _, s := range strings.Split(raw, " ") {
				if s = strings.TrimSpace(s); s != "" {
					cfg.ExtraDockerArgs = append(cfg.ExtraDockerArgs, s)
				}
			}
		}
		// Image-less jobs fall back to Shell by default so existing
		// YAML keeps working during rollout. Operators that want
		// "docker or bust" set GOCDNEXT_DOCKER_STRICT=true.
		var fallback engine.Engine
		if os.Getenv("GOCDNEXT_DOCKER_STRICT") != "true" {
			fallback = engine.NewShell()
		}
		logger.Info("agent engine: docker",
			"socket", cfg.SocketPath,
			"default_image", cfg.DefaultImage,
			"pull_policy", cfg.PullPolicy,
			"strict", fallback == nil)
		return engine.NewDocker(cfg, fallback), nil
	case "kubernetes":
		cfg := engine.KubernetesConfig{
			Namespace:          os.Getenv("GOCDNEXT_K8S_NAMESPACE"),
			KubeconfigPath:     os.Getenv("GOCDNEXT_KUBECONFIG"),
			WorkspacePVCName:   os.Getenv("GOCDNEXT_K8S_WORKSPACE_PVC"),
			WorkspaceMountPath: os.Getenv("GOCDNEXT_K8S_WORKSPACE_PATH"),
			DefaultImage:       os.Getenv("GOCDNEXT_K8S_DEFAULT_IMAGE"),
			AgentImage:         os.Getenv("GOCDNEXT_K8S_AGENT_IMAGE"),
			HousekeeperImage:   os.Getenv("GOCDNEXT_K8S_HOUSEKEEPER_IMAGE"),
		}
		// Job-pod scheduling baseline. Names use the JOB_ prefix
		// to avoid confusion with the agent's own pod nodeSelector
		// (set on the StatefulSet directly via Helm, not via these
		// envs). JSON shapes:
		//
		//   GOCDNEXT_K8S_JOB_NODE_SELECTOR={"workload":"ci"}
		//   GOCDNEXT_K8S_JOB_TOLERATIONS=[{"key":"ci-only","operator":"Equal","value":"true","effect":"NoSchedule"}]
		//
		// Empty / absent → no baseline; profile-scoped scheduling
		// still applies on a per-job basis. Malformed JSON fails
		// startup loud rather than silently shipping pods that
		// can't schedule.
		if raw := strings.TrimSpace(os.Getenv("GOCDNEXT_K8S_JOB_NODE_SELECTOR")); raw != "" {
			if err := json.Unmarshal([]byte(raw), &cfg.NodeSelector); err != nil {
				return nil, fmt.Errorf("GOCDNEXT_K8S_JOB_NODE_SELECTOR: invalid JSON object: %w", err)
			}
		}
		if raw := strings.TrimSpace(os.Getenv("GOCDNEXT_K8S_JOB_TOLERATIONS")); raw != "" {
			if err := json.Unmarshal([]byte(raw), &cfg.Tolerations); err != nil {
				return nil, fmt.Errorf("GOCDNEXT_K8S_JOB_TOLERATIONS: invalid JSON array: %w", err)
			}
		}
		mode := strings.ToLower(strings.TrimSpace(os.Getenv("GOCDNEXT_K8S_WORKSPACE_MODE")))
		switch mode {
		case "", "shared":
			cfg.WorkspaceMode = engine.WorkspaceModeShared
		case "isolated":
			cfg.WorkspaceMode = engine.WorkspaceModeIsolated
		default:
			return nil, fmt.Errorf("GOCDNEXT_K8S_WORKSPACE_MODE=%q not supported (use shared or isolated)", mode)
		}
		if cfg.WorkspaceMode == engine.WorkspaceModeIsolated {
			cfg.WorkspaceStorageClass = strings.TrimSpace(os.Getenv("GOCDNEXT_K8S_WORKSPACE_STORAGE_CLASS"))
			if v := strings.TrimSpace(os.Getenv("GOCDNEXT_K8S_WORKSPACE_SIZE")); v != "" {
				cfg.WorkspaceSize = v
			}
			if cfg.AgentImage == "" {
				return nil, fmt.Errorf(
					"GOCDNEXT_K8S_WORKSPACE_MODE=isolated requires GOCDNEXT_K8S_AGENT_IMAGE " +
						"so the init container can run `gocdnext-agent prep` " +
						"(the chart wires it to agent.image.repository:agent.image.tag)")
			}
		}
		if raw := os.Getenv("GOCDNEXT_K8S_IMAGE_PULL_SECRETS"); raw != "" {
			for _, s := range strings.Split(raw, ",") {
				if s = strings.TrimSpace(s); s != "" {
					cfg.ImagePullSecrets = append(cfg.ImagePullSecrets, s)
				}
			}
		}
		cfg.CleanupOnSuccess = os.Getenv("GOCDNEXT_K8S_CLEANUP_ON_SUCCESS") != "false"
		cfg.CleanupOnFailure = strings.EqualFold(os.Getenv("GOCDNEXT_K8S_CLEANUP_ON_FAILURE"), "true")
		cfg.ForceImagePullAlways = strings.EqualFold(os.Getenv("GOCDNEXT_K8S_FORCE_IMAGE_PULL_ALWAYS"), "true")

		eng, err := engine.NewKubernetes(cfg)
		if err != nil {
			return nil, fmt.Errorf("agent engine=kubernetes: %w", err)
		}
		logger.Info("agent engine: kubernetes",
			"namespace", cfg.Namespace,
			"workspace_mode", cfg.WorkspaceMode,
			"workspace_pvc", cfg.WorkspacePVCName,
			"default_image", cfg.DefaultImage)
		return eng, nil
	default:
		return nil, fmt.Errorf("GOCDNEXT_AGENT_ENGINE=%q not supported (use shell, docker, or kubernetes)", choice)
	}
}

// versionString returns a static version string until we wire ldflags at build
// time. Keeping it here lets us bump once instead of hunting the literal.
func versionString() string { return "0.1.0-dev" }

// runPrep is the entrypoint for `gocdnext-agent prep`, executed
// inside the "prep" init container of an isolated-mode job pod.
// Reads a JobAssignment protobuf blob from --assignment, materialises
// the workspace at --workspace (clone + artifact download), logs
// progress to stdout (k8s log collection picks it up). Returns
// non-nil error on any failure — main converts to non-zero exit so
// k8s reports the init container as failed.
func runPrep(args []string) error {
	fs := flag.NewFlagSet("prep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	assignmentPath := fs.String("assignment", "/etc/gocdnext/assignment.pb",
		"path to the serialised JobAssignment protobuf (mounted via Secret in isolated mode)")
	workspaceDir := fs.String("workspace", "/workspace",
		"directory where the workspace is materialised (must be the mount point of the pod's ephemeral PVC)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	data, err := os.ReadFile(*assignmentPath)
	if err != nil {
		return fmt.Errorf("read assignment %s: %w", *assignmentPath, err)
	}
	var a gocdnextv1.JobAssignment
	if err := proto.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("unmarshal assignment: %w", err)
	}

	// Propagate SIGINT/SIGTERM into the prep context so a pod
	// deletion (kubelet sends TERM, then KILL after grace) cleanly
	// aborts the in-flight git clone instead of leaving a half-tree.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runner.Prep(ctx, &a, *workspaceDir, os.Stdout)
}
