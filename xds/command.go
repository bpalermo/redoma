// Package xds is `redoma xds`, the host control plane for the agent
// containers. It serves the host Envoy, over ADS on a unix socket, the
// listeners of every running agent (LDS, bound inside the agent's network
// namespace), each agent's cluster to the relay (CDS) and each agent's
// SPIFFE identity (SDS), which it fetches from SPIRE on the agents' behalf:
// from the Delegated Identity API as the authorized delegate, or from the
// SPIFFE Broker API as its one broker (identity_api; see
// docs/broker-identity.md).
//
// It runs as uid 961 (redoma-sds) under systemd: see redoma-xds.service.
package xds

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/spf13/cobra"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/common/forbidden"
	"github.com/bpalermo/redoma/common/version"
	"github.com/bpalermo/redoma/xds/internal/activedir"
	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/cds"
	"github.com/bpalermo/redoma/xds/internal/controlplane"
	"github.com/bpalermo/redoma/xds/internal/fdstore"
	"github.com/bpalermo/redoma/xds/internal/hooksource"
	"github.com/bpalermo/redoma/xds/internal/hostcheck"
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
	identityAPI    string
	brokerSocket   string
	workloadAPI    string
	relay          cds.Relay
	// file is the settings file as read, before the flags: what the OCI
	// hook reads too.
	file config.Config
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
	fl.StringVar(&f.identityAPI, "identity-api", "", "how the agents' SVIDs are obtained: delegated or broker (default: the settings file's identity_api)")
	fl.StringVar(&f.brokerSocket, "spire-broker-socket", "", "the SPIRE agent's Broker API socket, in Broker mode (default: the settings file's spire_broker_socket)")
	fl.StringVar(&f.workloadAPI, "spire-workload-api", "", "the SPIRE agent's Workload API, where redoma xds gets its own SVID in Broker mode (default: the settings file's spire_workload_api)")
	fl.StringVar(&f.relay.Socket, "relay-socket", "/run/redoma-relay/relay.sock", "the relay's agent socket, as the host Envoy sees it; every agent gets a relay listener (127.0.0.1:7070) and a cluster to it. Empty: no relay")
	fl.StringVar(&f.relay.ID, "relay-id", "", "the relay's SPIFFE ID, which Envoy requires of its certificate (default: spiffe://<trust domain>/host/relay)")
	return cmd
}

// settle fills in what the command line left to the settings file: the
// trust domain, SPIRE's sockets and the identity API, and the relay's ID,
// derived from the trust domain finally chosen.
func (f *flags) settle(cmd *cobra.Command) error {
	conf, err := config.FromCommand(cmd)
	if err != nil {
		return err
	}
	f.file = conf
	config.Unless(cmd, "trust-domain", &f.cfg.TrustDomain, conf.TrustDomain)
	config.Unless(cmd, "spire-admin-socket", &f.adminSocket, conf.SpireAdminSocket)
	config.Unless(cmd, "identity-api", &f.identityAPI, conf.IdentityAPI)
	config.Unless(cmd, "spire-broker-socket", &f.brokerSocket, conf.SpireBrokerSocket)
	config.Unless(cmd, "spire-workload-api", &f.workloadAPI, conf.WorkloadAPI)
	config.Unless(cmd, "relay-id", &f.relay.ID, "spiffe://"+f.cfg.TrustDomain+config.RelayPath)
	// A flag is held to what the file is held to.
	conf.TrustDomain, conf.SpireAdminSocket = f.cfg.TrustDomain, f.adminSocket
	conf.IdentityAPI, conf.SpireBrokerSocket, conf.WorkloadAPI = f.identityAPI, f.brokerSocket, f.workloadAPI
	f.cfg.SVIDAfterBound = f.identityAPI == config.IdentityBroker
	if err := conf.Validate(); err != nil {
		return err
	}
	if f.relay.Socket != "" {
		f.cfg.Relay = &f.relay
	}
	return nil
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	log.Info("redoma xds starting", "version", version.Version, "xds_socket", f.xdsSocket, "identity_api", f.identityAPI, "trust_domain", f.cfg.TrustDomain)
	if f.cfg.Relay != nil {
		if err := checkRelayID(f.cfg.Relay.ID, f.cfg.TrustDomain); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	id, closeID, err := identityAPI(ctx, f, log)
	if err != nil {
		return err
	}
	defer closeID()

	snapshots := cache.NewSnapshotCache(true, controlplane.NodeHash{}, nil)
	l, err := server.Listen(f.xdsSocket)
	if err != nil {
		return err
	}

	source, err := sources(f, log)
	if err != nil {
		return err
	}
	ctl := controlplane.New(f.cfg, source, id, snapshots, log)

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

// identityAPI is the client of the API identity_api names, and what closes
// it. Broker mode first checks that the agents cannot reach SPIRE's sockets
// (brokerChecks).
func identityAPI(ctx context.Context, f flags, log *slog.Logger) (controlplane.Identity, func(), error) {
	if f.identityAPI != config.IdentityBroker {
		log.Info("the agents' SVIDs come from the Delegated Identity API", "spire_admin_socket", f.adminSocket)
		conn, err := identity.Dial(f.adminSocket)
		if err != nil {
			return nil, nil, err
		}
		return identity.New(conn, f.cfg.TrustDomain, log), func() { conn.Close() }, nil
	}
	log.Info("the agents' SVIDs come from the Broker API", "spire_broker_socket", f.brokerSocket, "spire_workload_api", f.workloadAPI)
	if err := brokerChecks(f, hostcheck.System); err != nil {
		return nil, nil, fmt.Errorf("broker mode: %w", err)
	}
	td, err := spiffeid.TrustDomainFromString(f.cfg.TrustDomain)
	if err != nil {
		return nil, nil, err
	}
	own := identity.OpenWorkload(ctx, f.workloadAPI, td, log)
	conn, err := identity.DialBroker(f.brokerSocket, own, td)
	if err != nil {
		own.Close()
		return nil, nil, err
	}
	return identity.NewBroker(conn, own, f.cfg.TrustDomain, log), func() { conn.Close(); own.Close() }, nil
}

// brokerChecks refuses Broker mode on a host where an agent could reach
// SPIRE's Workload API, and with it its own SVID (docs/broker-identity.md,
// "The mitigation"): the Workload API's and the Broker API's sockets must
// be where the OCI hook forbids every agent to mount, as the settings file
// the hook reads names them, and the Workload API's directory must be
// closed to all but the services.
func brokerChecks(f flags, acc hostcheck.Accounts) error {
	workload := strings.TrimPrefix(f.workloadAPI, "unix://")
	if err := hostcheck.SocketsForbidden([]string{workload, f.brokerSocket}, forbidden.Sources(f.file)); err != nil {
		return err
	}
	return hostcheck.WorkloadDir(filepath.Dir(workload), serviceUIDs(), acc)
}

// serviceUIDs may reach the Workload API: redoma xds's own, and the fixed
// ids of redoma-sds and redoma-relay (README.md).
func serviceUIDs() []uint32 {
	return []uint32{961, 963, uint32(os.Getuid())}
}

// sources is where the running agents come from: the launcher's state, and
// the OCI hook's registrations when the registration socket is set, with
// the pidfds systemd's store kept for them.
func sources(f flags, log *slog.Logger) (agent.Source, error) {
	active := f.active
	active.Interval, active.Log = f.cfg.ScanInterval, log
	store, stored, err := fdstore.FromEnv()
	if err != nil {
		log.Error("the descriptors systemd passed back", "err", err)
	}
	if store.Socket == "" {
		log.Warn("not started by systemd with a notify socket: the agents' pidfds do not survive a restart, and their containers then need one in Broker mode")
	}
	if f.registerSocket == "" {
		for _, files := range stored {
			for _, file := range files {
				file.Close()
			}
		}
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
		Store:        store,
		Stored:       stored,
	}
	return agent.Merge(log, &active, hooks), nil
}
