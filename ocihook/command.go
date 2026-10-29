// Package ocihook is `redoma oci-hook`, the OCI hook that registers a
// rootful container annotated redoma/agent=<name> (the settings file's
// agent_annotation) with redoma xds, so the host Envoy binds the agent's
// listeners in the container's network namespace and serves the agent's
// SPIFFE identity. See ocihook/hooks.d for the hook definitions.
//
//	redoma oci-hook create-runtime   (createRuntime stage)
//	redoma oci-hook poststop         (poststop stage)
//
// The runtime passes the container's state as JSON on stdin.
package ocihook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/user"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/ocihook/internal/hook"
)

type flags struct {
	socket     string
	netnsDir   string
	annotation string
	timeout    time.Duration
}

// Command is `redoma oci-hook`. The annotation defaults to what the
// settings file says.
func Command() *cobra.Command {
	var f flags
	cmd := &cobra.Command{
		Use:       "oci-hook create-runtime|poststop",
		Short:     "The OCI hook: register an annotated container as an agent, and unregister it",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"create-runtime", "poststop"},
		RunE: func(cmd *cobra.Command, args []string) error {
			conf, err := config.FromCommand(cmd)
			if err != nil {
				return err
			}
			config.Unless(cmd, "annotation", &f.annotation, conf.AgentAnnotation)
			return run(cmd.Context(), f, args[0], cmd.InOrStdin())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.socket, "socket", "/run/redoma-xds/register.sock", "redoma xds's registration socket")
	fl.StringVar(&f.netnsDir, "netns-dir", "/run/netns", "where the container's network namespace is mounted; the host Envoy must see mounts there")
	fl.StringVar(&f.annotation, "annotation", "", "the annotation naming the agent (default: the settings file's agent_annotation)")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "how long createRuntime waits for Envoy's listeners before failing the start")
	return cmd
}

func run(ctx context.Context, f flags, stage string, stdin io.Reader) error {
	var st hook.State
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&st); err != nil {
		return fmt.Errorf("reading the container state on stdin: %w", err)
	}
	h := &hook.Hook{
		Annotation: f.annotation,
		NetnsDir:   f.netnsDir,
		Timeout:    f.timeout,
		Mount:      hook.SystemMounter{},
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", f.socket)
		},
		Log:        os.Stderr,
		ServiceIDs: serviceIDs(system, os.Stderr),
	}
	switch stage {
	case "create-runtime":
		return h.CreateRuntime(ctx, st)
	case "poststop":
		return h.Poststop(ctx, st)
	default:
		return fmt.Errorf("unknown stage %q", stage)
	}
}

// lookups resolve a service's name, as a group and as a user, to its id.
type lookups struct {
	group func(name string) (string, error)
	user  func(name string) (string, error)
}

var system = lookups{
	group: func(name string) (string, error) {
		g, err := user.LookupGroup(name)
		if err != nil {
			return "", err
		}
		return g.Gid, nil
	},
	user: func(name string) (string, error) {
		u, err := user.Lookup(name)
		if err != nil {
			return "", err
		}
		return u.Uid, nil
	},
}

// serviceIDs are hook.ServiceIDs, always, and the ids the host's passwd and
// group files give the same names, should one have been made with another
// id. A name that is not there is no error; any other failure, and an id
// that is not a number, is logged: the static ids still hold.
func serviceIDs(l lookups, log io.Writer) map[uint32]string {
	ids := maps.Clone(hook.ServiceIDs)
	for _, name := range slices.Sorted(maps.Values(hook.ServiceIDs)) {
		for kind, lookup := range map[string]func(string) (string, error){"group": l.group, "user": l.user} {
			raw, err := lookup(name)
			var ug user.UnknownGroupError
			var uu user.UnknownUserError
			switch {
			case errors.As(err, &ug) || errors.As(err, &uu):
				continue
			case err != nil:
				fmt.Fprintf(log, "redoma oci-hook: looking up the %s %s: %v; refusing its static id only\n", kind, name, err)
				continue
			}
			id, err := strconv.ParseUint(raw, 10, 32)
			if err != nil {
				fmt.Fprintf(log, "redoma oci-hook: the %s %s has id %q, not a number; refusing its static id only\n", kind, name, raw)
				continue
			}
			ids[uint32(id)] = name
		}
	}
	return ids
}
