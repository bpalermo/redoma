// Package xds is `redoma xds`, the host control plane for the agent
// containers. It serves the host Envoy, over ADS on a unix socket, the
// listeners of every running agent (LDS, bound inside the agent's network
// namespace), each agent's cluster to the relay (CDS) and each agent's
// SPIFFE identity (SDS), which it fetches from SPIRE's Delegated Identity
// API as the authorized delegate.
//
// It runs as uid 961 (redoma-sds) under systemd: see redoma-xds.service.
package xds

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/spf13/cobra"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/common/version"
	"github.com/bpalermo/redoma/xds/internal/activedir"
	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/cds"
	"github.com/bpalermo/redoma/xds/internal/controlplane"
	"github.com/bpalermo/redoma/xds/internal/hooksource"
	"github.com/bpalermo/redoma/xds/internal/identity"
	"github.com/bpalermo/redoma/xds/internal/server"
)

type flags struct {
	cfg            controlplane.Config
	active         activedir.Source
	allowlist      string
	registrations  string
	boundTimeout   time.Duration
	registerSocket string
	xdsSocket      string
	adminSocket    string
	relay          cds.Relay
}

// Command is `redoma xds`. The trust domain, the relay's ID and the SPIRE
// admin socket default to what the settings file says.
func Command() *cobra.Command {
	return command(&flags{})
}

// command is Command, its flags parsed into f.
func command(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "xds",
		Short: "Serve the host Envoy the agents' listeners, relay clusters and identities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.settle(cmd); err != nil {
				return err
			}
			return run(cmd.Context(), *f, slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.active.ActiveDir, "active-dir", "/run/redoma/active", "one empty file per agent a launcher runs, named after the agent")
	fl.StringVar(&f.active.NetnsDir, "netns-dir", "/run/netns", "where the agents' network namespaces are: redoma-<name> for a launcher's agent, redoma-oci-<container> for the hook's")
	fl.StringVar(&f.cfg.EtcDir, "etc-dir", "/etc/redoma", "holds egress/_default.txt and egress/<name>.txt, the agents' CONNECT allowlists, and model-auth")
	fl.StringVar(&f.cfg.TrustDomain, "trust-domain", "", "the agents' SPIFFE trust domain (default: the settings file's trust_domain)")
	fl.DurationVar(&f.cfg.ScanInterval, "scan-interval", time.Second, "how often the running agents and their allowlists are read")
	fl.DurationVar(&f.cfg.RebindTimeout, "rebind-timeout", 5*time.Second, "how long the removal of the listeners at a replaced namespace path waits for Envoy to report it applied before it is published again")
	fl.DurationVar(&f.cfg.RebindGrace, "rebind-grace", 500*time.Millisecond, "how long after Envoy reports that removal applied before the agent is bound there again (Envoy closes the old sockets asynchronously)")
	fl.StringVar(&f.cfg.StateFile, "state-file", "/run/redoma-xds/state.json", "records which namespace Envoy may hold listeners in at each path, so a restart notices a namespace replaced meanwhile; keep it on a tmpfs cleared at boot")
	fl.DurationVar(&f.cfg.SyncTimeout, "sync-timeout", 10*time.Second, "how long the first snapshot waits for the agent source to report its initial set; then it publishes what it has, unless the state file records listeners Envoy may still hold, in which case it logs an error and waits again")
	fl.StringVar(&f.registerSocket, "register-socket", "/run/redoma-xds/register.sock", "the OCI hook's registration socket (root only); empty: no hook registrations")
	fl.StringVar(&f.allowlist, "allowlist", "/etc/redoma/agents.txt", "the agent names a hook-registered container may claim, one per line")
	fl.StringVar(&f.registrations, "registrations-file", "/run/redoma-xds/registrations.json", "records the hook registrations across restarts; keep it on a tmpfs cleared at boot")
	fl.DurationVar(&f.boundTimeout, "bound-timeout", 25*time.Second, "how long a hook registration waits for Envoy to bind the listeners (below the hook's own --timeout)")
	fl.StringVar(&f.xdsSocket, "xds-socket", "/run/redoma-xds/xds.sock", "the unix socket the host Envoy's ADS cluster connects to")
	fl.StringVar(&f.adminSocket, "spire-admin-socket", "", "the SPIRE agent's admin socket, the Delegated Identity API (default: the settings file's spire_admin_socket)")
	fl.StringVar(&f.relay.Socket, "relay-socket", "/run/redoma-relay/relay.sock", "the relay's agent socket, as the host Envoy sees it; every agent gets a relay listener (127.0.0.1:7070) and a cluster to it. Empty: no relay")
	fl.StringVar(&f.relay.ID, "relay-id", "", "the relay's SPIFFE ID, which Envoy requires of its certificate (default: spiffe://<trust domain>/host/relay)")
	return cmd
}

// settle fills in what the command line left to the settings file: the
// trust domain, the SPIRE admin socket, and the relay's ID, derived from
// the trust domain finally chosen.
func (f *flags) settle(cmd *cobra.Command) error {
	conf, err := config.FromCommand(cmd)
	if err != nil {
		return err
	}
	config.Unless(cmd, "trust-domain", &f.cfg.TrustDomain, conf.TrustDomain)
	config.Unless(cmd, "spire-admin-socket", &f.adminSocket, conf.SpireAdminSocket)
	config.Unless(cmd, "relay-id", &f.relay.ID, "spiffe://"+f.cfg.TrustDomain+config.RelayPath)
	// A flag is held to what the file is held to.
	conf.TrustDomain, conf.SpireAdminSocket = f.cfg.TrustDomain, f.adminSocket
	if err := conf.Validate(); err != nil {
		return err
	}
	if f.relay.Socket != "" {
		f.cfg.Relay = &f.relay
	}
	return nil
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	log.Info("redoma xds starting", "version", version.Version, "xds_socket", f.xdsSocket, "spire_admin_socket", f.adminSocket, "trust_domain", f.cfg.TrustDomain)
	if f.cfg.Relay != nil {
		if err := checkRelayID(f.cfg.Relay.ID, f.cfg.TrustDomain); err != nil {
			return err
		}
	}
	conn, err := identity.Dial(f.adminSocket)
	if err != nil {
		return err
	}
	defer conn.Close()

	snapshots := cache.NewSnapshotCache(true, controlplane.NodeHash{}, nil)
	l, err := server.Listen(f.xdsSocket)
	if err != nil {
		return err
	}

	source, err := sources(f, log)
	if err != nil {
		return err
	}
	ctl := controlplane.New(f.cfg, source, identity.New(conn, f.cfg.TrustDomain, log), snapshots, log)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- ctl.Run(ctx) }()
	go func() { errc <- server.Serve(ctx, l, snapshots, server.ListenerVersions(ctl.ListenersApplied), log) }()

	// The first to return ends both: a control plane that stopped publishing
	// must not keep serving a snapshot that no longer follows the agents.
	err = <-errc
	cancel()
	if err2 := <-errc; err == nil {
		err = err2
	}
	return err
}

// checkRelayID refuses a --relay-id that is not exactly the relay's:
// spiffe://<td>/host/relay. The spelling is canonical because FromString
// is strict: it refuses upper case, a port, userinfo, a query, a fragment,
// dot segments, empty segments and percent-encoding, rather than
// normalizing them. The bundle is a root shared with other trust domains,
// and every workload of this one chains to it too, so the ID Envoy pins is
// what tells the relay from an agent (/agent/<name>), the delegate
// (/host/sds) or anyone else.
func checkRelayID(id, trustDomain string) error {
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return fmt.Errorf("--trust-domain %q: %w", trustDomain, err)
	}
	parsed, err := spiffeid.FromString(id)
	if err != nil {
		return fmt.Errorf("--relay-id %q: %w", id, err)
	}
	if !parsed.MemberOf(td) || parsed.Path() != config.RelayPath {
		return fmt.Errorf("--relay-id %q is not the relay's ID, spiffe://%s%s", id, td, config.RelayPath)
	}
	return nil
}

// sources is where the running agents come from: the launcher's state, and
// the OCI hook's registrations when the registration socket is set.
func sources(f flags, log *slog.Logger) (agent.Source, error) {
	active := f.active
	active.Interval, active.Log = f.cfg.ScanInterval, log
	if f.registerSocket == "" {
		return &active, nil
	}
	l, err := hooksource.Listen(f.registerSocket)
	if err != nil {
		return nil, err
	}
	hooks := &hooksource.Source{
		Listener:     l,
		Allowlist:    f.allowlist,
		NetnsDir:     f.active.NetnsDir,
		StateFile:    f.registrations,
		Interval:     f.cfg.ScanInterval,
		BoundTimeout: f.boundTimeout,
		Log:          log,
	}
	return agent.Merge(log, &active, hooks), nil
}
