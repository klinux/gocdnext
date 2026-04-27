// Command gocdnext is the CLI: validate pipelines, run locally, manage resources.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/gocdnext/gocdnext/cli/internal/admin"
	"github.com/gocdnext/gocdnext/cli/internal/apply"
	"github.com/gocdnext/gocdnext/cli/internal/secrets"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "gocdnext",
		Short:         "gocdnext CLI",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		validateCmd(),
		runLocalCmd(),
		applyCmd(),
		secretCmd(),
		adminCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [path]",
		Short: "Validate .gocdnext/ pipelines",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			fmt.Printf("TODO: validate %s\n", path)
			return nil
		},
	}
}

func runLocalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run-local [file]",
		Short: "Run a pipeline locally (requires Docker)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println("TODO: run-local")
			return nil
		},
	}
}

func applyCmd() *cobra.Command {
	var (
		slug             string
		name             string
		description      string
		configRepo       string
		serverURL        string
		scmURL           string
		scmProvider      string
		scmDefaultBranch string
		scmWebhookSecret string
	)
	cmd := &cobra.Command{
		Use:   "apply [path]",
		Short: "Upload .gocdnext/ pipelines to the server",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			if slug == "" {
				return fmt.Errorf("--slug is required")
			}
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}

			files, err := apply.ReadFolder(path)
			if err != nil {
				return err
			}

			var scm *apply.SCMSource
			if scmURL != "" || scmProvider != "" {
				if scmURL == "" || scmProvider == "" {
					return fmt.Errorf("--scm-url and --scm-provider must be set together")
				}
				scm = &apply.SCMSource{
					Provider:      scmProvider,
					URL:           scmURL,
					DefaultBranch: scmDefaultBranch,
					WebhookSecret: scmWebhookSecret,
				}
			}

			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			resp, err := apply.Post(ctx, &http.Client{Timeout: 30 * time.Second}, serverURL, apply.Request{
				Slug:        slug,
				Name:        name,
				Description: description,
				ConfigRepo:  configRepo,
				Files:       files,
				SCMSource:   scm,
			})
			if err != nil {
				return err
			}

			printResult(c.OutOrStdout(), resp)
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "project slug (required)")
	cmd.Flags().StringVar(&name, "name", "", "project display name (defaults to slug)")
	cmd.Flags().StringVar(&description, "description", "", "project description")
	cmd.Flags().StringVar(&configRepo, "config-repo", "", "config repo URL (for audit)")
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	cmd.Flags().StringVar(&scmURL, "scm-url", "", "SCM repo URL hosting .gocdnext/ (binds the project to an scm_source)")
	cmd.Flags().StringVar(&scmProvider, "scm-provider", "", "SCM provider: github | gitlab | bitbucket | manual (required with --scm-url)")
	cmd.Flags().StringVar(&scmDefaultBranch, "scm-default-branch", "main", "default branch for the scm source")
	cmd.Flags().StringVar(&scmWebhookSecret, "scm-webhook-secret", "", "HMAC secret expected on webhooks (will later drive config-drift re-sync)")
	_ = cmd.MarkFlagRequired("slug")
	cmd.SetContext(context.Background())
	return cmd
}

func printResult(w interface {
	Write(p []byte) (int, error)
}, r apply.Response) {
	prefix := "updated"
	if r.ProjectCreated {
		prefix = "created"
	}
	fmt.Fprintf(w, "project %s %s\n", prefix, r.ProjectID)
	if r.SCMSource != nil {
		action := "updated"
		if r.SCMSource.Created {
			action = "created"
		}
		fmt.Fprintf(w, "  scm_source %s %s (%s, %s)\n",
			action, r.SCMSource.Provider, r.SCMSource.URL, r.SCMSource.DefaultBranch)
	}
	for _, p := range r.Pipelines {
		action := "updated"
		if p.Created {
			action = "created"
		}
		fmt.Fprintf(w, "  pipeline %s %s (materials: +%d -%d)\n",
			action, p.Name, p.MaterialsAdded, p.MaterialsRemoved)
	}
	for _, name := range r.PipelinesRemoved {
		fmt.Fprintf(w, "  pipeline removed %s\n", name)
	}
}

func secretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage project secrets (encrypted at rest by the server)",
	}
	cmd.AddCommand(secretSetCmd(), secretListCmd(), secretRmCmd())
	return cmd
}

func secretSetCmd() *cobra.Command {
	var (
		slug       string
		serverURL  string
		fromStdin  bool
		fromFile   string
	)
	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Set a secret. Value is read from stdin/file/TTY — never from a flag.",
		Long: strings.TrimSpace(`
Set a project secret. The value is read from one of:
  * --from-file PATH     (read file contents verbatim)
  * stdin when it's piped (cat token.txt | gocdnext secret set --slug X NAME)
  * an interactive TTY prompt (silent, like sudo)

Refusing to accept the value from a flag is deliberate: it keeps secrets out of
shell history and out of 'ps'.
`),
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			if slug == "" {
				return fmt.Errorf("--slug is required")
			}
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}

			value, err := readSecretValue(fromStdin, fromFile)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			resp, err := secrets.Set(ctx, &http.Client{Timeout: 30 * time.Second}, serverURL, slug,
				secrets.SetRequest{Name: name, Value: value})
			if err != nil {
				return err
			}
			action := "updated"
			if resp.Created {
				action = "created"
			}
			fmt.Fprintf(c.OutOrStdout(), "secret %s %s\n", action, resp.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "project slug (required)")
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	cmd.Flags().BoolVar(&fromStdin, "from-stdin", false, "force reading value from stdin even when a TTY is attached")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read value from the given file path (use - for stdin)")
	_ = cmd.MarkFlagRequired("slug")
	cmd.SetContext(context.Background())
	return cmd
}

func secretListCmd() *cobra.Command {
	var (
		slug      string
		serverURL string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List secret names and timestamps for a project (no values)",
		RunE: func(c *cobra.Command, _ []string) error {
			if slug == "" {
				return fmt.Errorf("--slug is required")
			}
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			list, err := secrets.List(ctx, &http.Client{Timeout: 15 * time.Second}, serverURL, slug)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "(no secrets)")
				return nil
			}
			for _, s := range list {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\n", s.Name, s.UpdatedAt)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "project slug (required)")
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	_ = cmd.MarkFlagRequired("slug")
	cmd.SetContext(context.Background())
	return cmd
}

func secretRmCmd() *cobra.Command {
	var (
		slug      string
		serverURL string
	)
	cmd := &cobra.Command{
		Use:   "rm NAME",
		Short: "Remove a secret from a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			if slug == "" {
				return fmt.Errorf("--slug is required")
			}
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if err := secrets.Delete(ctx, &http.Client{Timeout: 15 * time.Second}, serverURL, slug, name); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "secret removed %s\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "project slug (required)")
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	_ = cmd.MarkFlagRequired("slug")
	cmd.SetContext(context.Background())
	return cmd
}

// readSecretValue resolves the secret plaintext from one of: file, piped
// stdin, or an interactive TTY prompt (silent input, like sudo password).
// The value may legitimately be multi-line (PEM keys) — stdin and file read
// the full content verbatim; the TTY prompt strips a single trailing newline
// from ReadPassword.
func readSecretValue(fromStdin bool, fromFile string) (string, error) {
	if fromFile != "" {
		if fromFile == "-" {
			return readAllTrim(os.Stdin)
		}
		b, err := os.ReadFile(fromFile)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", fromFile, err)
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	// Piped stdin (e.g., `cat token.txt | gocdnext secret set ...`)
	if fromStdin || !term.IsTerminal(int(os.Stdin.Fd())) {
		return readAllTrim(os.Stdin)
	}
	// Interactive TTY — silent prompt.
	fmt.Fprint(os.Stderr, "Value: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read tty: %w", err)
	}
	return string(b), nil
}

func readAllTrim(r io.Reader) (string, error) {
	b, err := io.ReadAll(bufio.NewReader(r))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// adminCmd groups ops-level actions that bypass the HTTP API.
// Today that's local-user provisioning. Runs against the same DB
// the server uses (GOCDNEXT_DATABASE_URL); the server does not
// have to be running.
func adminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Ops commands that talk to the DB directly (local users, etc.)",
	}
	cmd.AddCommand(adminCreateUserCmd(), adminResetPasswordCmd())
	return cmd
}

func adminCreateUserCmd() *cobra.Command {
	var (
		email       string
		name        string
		role        string
		databaseURL string
		fromStdin   bool
		fromFile    string
	)
	cmd := &cobra.Command{
		Use:   "create-user",
		Short: "Create (or rotate) a local-password user",
		Long: strings.TrimSpace(`
Create a local-password user directly in the database. Use this
to bootstrap the first admin before any OIDC provider is wired,
or as a break-glass account for oncall.

Password is read from:
  * --from-file PATH       (read file contents verbatim)
  * stdin when it's piped  (echo 'pw' | gocdnext admin create-user ...)
  * an interactive TTY prompt (silent, like sudo)

If a local user with this email already exists, the password +
role + name are rotated.

Requires GOCDNEXT_DATABASE_URL (or --database-url) with write
access to the users table.
`),
		RunE: func(c *cobra.Command, _ []string) error {
			if email == "" {
				return fmt.Errorf("--email is required")
			}
			if databaseURL == "" {
				databaseURL = envOr("GOCDNEXT_DATABASE_URL", "")
			}
			if databaseURL == "" {
				return fmt.Errorf("--database-url or GOCDNEXT_DATABASE_URL is required")
			}
			password, err := readSecretValue(fromStdin, fromFile)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			created, err := admin.CreateOrUpdateLocalUser(ctx, databaseURL, email, name, role, password)
			if err != nil {
				return err
			}
			verb := "rotated"
			if created {
				verb = "created"
			}
			fmt.Fprintf(c.OutOrStdout(), "local user %s %s (role=%s)\n", verb, email, role)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "user email (also the login name)")
	cmd.Flags().StringVar(&name, "name", "", "display name (defaults to the email local-part)")
	cmd.Flags().StringVar(&role, "role", admin.RoleAdmin, "admin | user | viewer")
	cmd.Flags().StringVar(&databaseURL, "database-url", "", "Postgres URL (env GOCDNEXT_DATABASE_URL)")
	cmd.Flags().BoolVar(&fromStdin, "from-stdin", false, "force reading password from stdin even when a TTY is attached")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read password from the given file path (use - for stdin)")
	_ = cmd.MarkFlagRequired("email")
	cmd.SetContext(context.Background())
	return cmd
}

func adminResetPasswordCmd() *cobra.Command {
	var (
		email       string
		databaseURL string
		fromStdin   bool
		fromFile    string
	)
	cmd := &cobra.Command{
		Use:   "reset-password",
		Short: "Rotate the password for an existing local user",
		RunE: func(c *cobra.Command, _ []string) error {
			if email == "" {
				return fmt.Errorf("--email is required")
			}
			if databaseURL == "" {
				databaseURL = envOr("GOCDNEXT_DATABASE_URL", "")
			}
			if databaseURL == "" {
				return fmt.Errorf("--database-url or GOCDNEXT_DATABASE_URL is required")
			}
			password, err := readSecretValue(fromStdin, fromFile)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			if err := admin.ResetPassword(ctx, databaseURL, email, password); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "password rotated for %s\n", email)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "user email")
	cmd.Flags().StringVar(&databaseURL, "database-url", "", "Postgres URL (env GOCDNEXT_DATABASE_URL)")
	cmd.Flags().BoolVar(&fromStdin, "from-stdin", false, "force reading password from stdin even when a TTY is attached")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read password from the given file path (use - for stdin)")
	_ = cmd.MarkFlagRequired("email")
	cmd.SetContext(context.Background())
	return cmd
}
