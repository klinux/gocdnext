// Command gocdnext is the CLI: validate pipelines, run locally, manage resources.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [file]",
		Short: "Validate a .gocdnext.yaml file",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// TODO(phase-1): import parser (avoiding cycle via separate sharedlib module).
			path := ".gocdnext.yaml"
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
