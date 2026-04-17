// Command gocdnext is the CLI: validate pipelines, run locally, manage resources.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gocdnext/gocdnext/cli/internal/apply"
)

func main() {
	root := &cobra.Command{
		Use:           "gocdnext",
		Short:         "gocdnext CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		validateCmd(),
		runLocalCmd(),
		applyCmd(),
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
		slug        string
		name        string
		description string
		configRepo  string
		serverURL   string
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

			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			resp, err := apply.Post(ctx, &http.Client{Timeout: 30 * time.Second}, serverURL, apply.Request{
				Slug:        slug,
				Name:        name,
				Description: description,
				ConfigRepo:  configRepo,
				Files:       files,
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
