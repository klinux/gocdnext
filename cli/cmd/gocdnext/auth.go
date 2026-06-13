package main

import (
	"context"
	"fmt"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/gocdnext/gocdnext/cli/internal/cliconfig"
)

func loginCmd() *cobra.Command {
	var (
		serverURL string
		fromStdin bool
		fromFile  string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store an API token for a server. Token is read from stdin/file/TTY — never from a flag.",
		Long: strings.TrimSpace(`
Authenticate the CLI against a gocdnext server using an API token
(create one in the web UI under Account → API tokens).

The token is read from one of:
  * --from-file PATH     (read file contents verbatim)
  * stdin when it's piped (cat token.txt | gocdnext login --server ...)
  * an interactive TTY prompt (silent, like sudo)

Refusing to accept the token from a flag is deliberate: it keeps it out
of shell history and out of 'ps'.

The token is validated against the server before being saved to
~/.config/gocdnext/config.json (0600), keyed by server URL — you can be
logged into several servers at once. For CI and bots, set the
GOCDNEXT_TOKEN environment variable instead; it overrides the file.
`),
		RunE: func(c *cobra.Command, _ []string) error {
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}
			token, err := readSecretValue(fromStdin, fromFile)
			if err != nil {
				return err
			}
			if token == "" {
				return fmt.Errorf("empty token")
			}

			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			who, err := cliconfig.ValidateToken(ctx, serverURL, token)
			if err != nil {
				return fmt.Errorf("login to %s failed: %w", serverURL, err)
			}
			if err := cliconfig.SetToken(serverURL, token); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "logged in to %s as %s\n", serverURL, who)
			return nil
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	cmd.Flags().BoolVar(&fromStdin, "from-stdin", false, "force reading token from stdin even when a TTY is attached")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read token from the given file path (use - for stdin)")
	cmd.SetContext(context.Background())
	return cmd
}

func logoutCmd() *cobra.Command {
	var serverURL string
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored API token for a server",
		RunE: func(c *cobra.Command, _ []string) error {
			if serverURL == "" {
				serverURL = envOr("GOCDNEXT_SERVER_URL", "http://localhost:8153")
			}
			if err := cliconfig.DeleteToken(serverURL); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "logged out of %s\n", serverURL)
			fmt.Fprintln(c.OutOrStdout(), "note: this removes the local copy; revoke the token itself in the web UI")
			return nil
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "gocdnext server URL (env GOCDNEXT_SERVER_URL)")
	cmd.SetContext(context.Background())
	return cmd
}

// authHint enriches 401 errors from any server-facing command with the
// way out. The HTTP helpers format upstream failures as
// "server returned <code>: ...".
func authHint(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "server returned 401") {
		return fmt.Errorf("%w\nhint: authenticate with `gocdnext login --server <url>` (or set %s)", err, cliconfig.EnvToken)
	}
	return err
}
