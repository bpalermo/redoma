// Command redoma is the host side of redoma: one binary, one subcommand per
// role.
//
//	redoma xds        the control plane of the host Envoy (redoma-xds.service)
//	redoma relay      the relay between the lead and the agents (redoma-relay.service)
//	redoma msg        the lead's command line for the relay
//	redoma mcp        the same as MCP tools on stdio
//	redoma oci-hook   the OCI hook that registers annotated containers
//
// Every subcommand reads the host's settings, /etc/redoma/redoma.conf
// (--config), and its own flags override them.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/common/version"
	"github.com/bpalermo/redoma/msg"
	"github.com/bpalermo/redoma/ocihook"
	"github.com/bpalermo/redoma/relay"
	"github.com/bpalermo/redoma/xds"
)

// root is the redoma command and its subcommands.
func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "redoma",
		Short:   "Isolated coding-agent containers behind one host Envoy",
		Version: version.Version,
		// A failure is printed once, by main, without the usage.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.PersistentFlags().String(config.FlagName, config.DefaultPath, "the host's settings file; a file given here must exist")
	cmd.AddCommand(xds.Command(), relay.Command(), msg.Command(), msg.MCPCommand(), ocihook.Command())
	return cmd
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	cmd, err := root().ExecuteContextC(ctx)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd.CommandPath(), err)
		os.Exit(1)
	}
}
