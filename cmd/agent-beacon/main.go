// Command agent-beacon runs the dashboard server or the session wrapper,
// selected by subcommand.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "agent-beacon",
		Short:         "Coding-agent fleet dashboard",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(serverCmd(), sessionCmd(), monitorCmd(), reportCmd(), installCmd(), uninstallCmd(), orchestrateCmd(), versionCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Println("agent-beacon", version)
		},
	}
}
