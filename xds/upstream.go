package xds

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/bpalermo/redoma/xds/internal/mcpconf"
	"github.com/bpalermo/redoma/xds/internal/upstreamrun"
)

// copyToken copies u's credential from source. Every start of the upstream
// runs it: one without a token_file has nothing to copy, and must not fail
// that start.
func copyToken(u *mcpconf.Upstream, source string, owner uint32, log io.Writer) error {
	if u.TokenFile == "" {
		fmt.Fprintf(log, "upstream %s has no token_file: nothing to copy\n", u.Name)
		return nil
	}
	return upstreamrun.CopyToken(source, u.TokenFile, owner)
}

// UpstreamCommand is `redoma mcp-upstream`, what a host MCP upstream's units
// run (docs/mcp-upstreams.md):
//
//	redoma mcp-upstream run <name>             redoma-mcp@<name>.service
//	redoma mcp-upstream token --owner=<uid> <name> <source>
//	                                           redoma-mcp-token@<name>.service
func UpstreamCommand() *cobra.Command {
	var etc, podman string
	var owner uint32
	cmd := &cobra.Command{
		Use:   "mcp-upstream",
		Short: "Run a host MCP upstream's container, and copy its credential",
	}
	cmd.PersistentFlags().StringVar(&etc, "etc-dir", "/etc/redoma", "holds mcp/<name>.conf, the upstreams' configuration")
	read := func(name string) (*mcpconf.Upstream, error) {
		u, err := mcpconf.Dir{Etc: etc}.ReadUpstream(name)
		if err == nil && u == nil {
			err = fmt.Errorf("no %s", mcpconf.Dir{Etc: etc}.UpstreamPath(name))
		}
		return u, err
	}
	run := &cobra.Command{
		Use:   "run <name>",
		Short: "Run the upstream's container in the foreground, under Podman, as its .conf says",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := read(args[0])
			if err != nil {
				return err
			}
			if err := upstreamrun.CheckToken(u); err != nil {
				return err
			}
			// Podman in this process's place, so the unit's process is the
			// container's.
			return syscall.Exec(podman, upstreamrun.PodmanArgs(u), os.Environ())
		},
	}
	run.Flags().StringVar(&podman, "podman", "/usr/bin/podman", "the podman binary")
	token := &cobra.Command{
		Use:   "token <name> <source>",
		Short: "Copy the upstream's credential from source, where the host's minter writes it, to its token_file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := read(args[0])
			if err != nil {
				return err
			}
			return copyToken(u, args[1], owner, cmd.ErrOrStderr())
		},
	}
	token.Flags().Uint32Var(&owner, "owner", 0, "the minter's uid: the source must be its file, on a path of directories only root and it can write")
	cmd.AddCommand(run, token)
	return cmd
}
