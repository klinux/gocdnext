package main

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gocdnext/gocdnext/cli/internal/cliconfig"
	"github.com/gocdnext/gocdnext/cli/internal/compliance"
)

// complianceEntryPrefix mirrors server/pkg/compliance.ReservedPrefix — a job or
// stage carrying it was contributed by a policy (enforced).
const complianceEntryPrefix = "_compliance_"

// complianceCmd groups the read-only compliance inspection commands. Listing
// and previewing only — frameworks/policies are authored in the dashboard
// (admin-only, separation of duties).
func complianceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compliance",
		Short: "Inspect compliance frameworks, policies, and a project's effective pipeline (read-only)",
	}
	cmd.AddCommand(
		complianceFrameworksCmd(),
		compliancePoliciesCmd(),
		complianceEffectiveCmd(),
	)
	return cmd
}

func complianceFrameworksCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "frameworks", Short: "Compliance frameworks"}
	cmd.AddCommand(complianceFrameworksListCmd())
	return cmd
}

func complianceFrameworksListCmd() *cobra.Command {
	var serverURL string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List compliance frameworks",
		RunE: func(c *cobra.Command, _ []string) error {
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			fws, err := compliance.ListFrameworks(ctx, cliconfig.HTTPClient(serverURL, 15*time.Second), serverURL)
			if err != nil {
				return authHint(err)
			}
			if len(fws) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "(no frameworks)")
				return nil
			}
			// id first: it's what `effective-pipeline --frameworks` consumes.
			fmt.Fprintln(c.OutOrStdout(), "ID\tNAME\tDESCRIPTION")
			for _, f := range fws {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\n", f.ID, f.Name, f.Description)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	cmd.SetContext(context.Background())
	return cmd
}

func compliancePoliciesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "policies", Short: "Compliance policies"}
	cmd.AddCommand(compliancePoliciesListCmd())
	return cmd
}

func compliancePoliciesListCmd() *cobra.Command {
	var serverURL string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List compliance policies (metadata)",
		RunE: func(c *cobra.Command, _ []string) error {
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			policies, err := compliance.ListPolicies(ctx, cliconfig.HTTPClient(serverURL, 15*time.Second), serverURL)
			if err != nil {
				return authHint(err)
			}
			if len(policies) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "(no policies)")
				return nil
			}
			for _, p := range policies {
				targets := "framework-scoped"
				if p.AppliesToAll {
					targets = "all-projects"
				}
				state := "enabled"
				if !p.Enabled {
					state = "disabled"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\tpriority=%d\t%s\t%s\n",
					p.Name, p.Mode, p.Priority, targets, state)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	cmd.SetContext(context.Background())
	return cmd
}

func complianceEffectiveCmd() *cobra.Command {
	var (
		serverURL  string
		frameworks string
	)
	cmd := &cobra.Command{
		Use:   "effective-pipeline SLUG",
		Short: "Preview a project's effective (post-policy) pipeline definition",
		Long: strings.TrimSpace(`
Show, per pipeline of a project, the effective definition every run uses after
compliance policies are merged. Jobs/stages contributed by a policy carry the
reserved _compliance_ prefix and are marked [enforced]; the server-owned
synthetic pipeline is marked [server-managed].

Without --frameworks it reads the stored effective definition (what runs today).
With --frameworks a,b it is a what-if recompute for that hypothetical framework
set (nothing is persisted); --frameworks="" previews "no frameworks".
`),
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}
			var whatIf *string
			if c.Flags().Changed("frameworks") {
				whatIf = &frameworks
			}
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			views, err := compliance.EffectivePipeline(
				ctx, cliconfig.HTTPClient(serverURL, 15*time.Second), serverURL, args[0], whatIf)
			if err != nil {
				return authHint(err)
			}
			printEffectivePipelines(c.OutOrStdout(), views)
			return nil
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	cmd.Flags().StringVar(&frameworks, "frameworks", "",
		"what-if: comma-separated framework ids to preview (flag present = what-if; empty = none)")
	cmd.SetContext(context.Background())
	return cmd
}

func printEffectivePipelines(w io.Writer, views []compliance.PipelineView) {
	if len(views) == 0 {
		fmt.Fprintln(w, "(no pipelines)")
		return
	}
	for i, v := range views {
		if i > 0 {
			fmt.Fprintln(w)
		}
		suffix := ""
		if v.SystemManaged {
			suffix = " [server-managed]"
		}
		fmt.Fprintf(w, "%s%s\n", v.Name, suffix)
		if len(v.Effective.Stages) > 0 {
			fmt.Fprintf(w, "  stages: %s\n", strings.Join(v.Effective.Stages, ", "))
		}
		for _, j := range v.Effective.Jobs {
			mark := ""
			if strings.HasPrefix(j.Name, complianceEntryPrefix) {
				mark = " [enforced]"
			}
			fmt.Fprintf(w, "  - %s (%s)%s\n", j.Name, j.Stage, mark)
		}
	}
}
