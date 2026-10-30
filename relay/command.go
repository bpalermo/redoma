// Package relay is `redoma relay`, which carries messages between the lead
// and the agents in their containers. Each agent's redoma-shim holds one stream to
// it, through the host Envoy, which connects here over mutual TLS with the
// agent's SVID: the client certificate is the agent's identity. The lead
// talks to the second socket, a local gRPC API (redoma msg and redoma mcp).
//
// It runs as its own system user under systemd: see redoma-relay.service.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
	"github.com/bpalermo/redoma/common/allowlist"
	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/common/version"
	"github.com/bpalermo/redoma/relay/internal/hub"
	"github.com/bpalermo/redoma/relay/internal/mtls"
	"github.com/bpalermo/redoma/relay/internal/svid"
)

// SocketMode is both sockets' mode. Each socket's owner is the relay and its
// group is set explicitly after the bind, never inherited from the
// directory: the agents' socket's is --socket-group (redoma-relay, 963),
// which the host Envoy's container joins, and whoever reaches it still needs
// an agent's SVID; the lead's is --lead-group (redoma-lead, 964), and is
// the whole of the lead's authority: whoever connects can task any agent.
// The two groups must differ, or the lead's users could dial the agents'
// socket.
const SocketMode = 0o660

// Keepalive on the hop from Envoy: pings every 30s, a connection dead after
// 10s more without an answer, and Envoy's own pings (30s) let through.
var (
	serverKeepalive = keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}
	enforcement     = keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}
)

type flags struct {
	socket      string
	socketGroup string
	leadSocket  string
	leadGroup   string
	stateDir    string
	workloadAPI string
	allowlist   string
	trustDomain string
	ackEvery    time.Duration
}

// Command is `redoma relay`. The trust domain and the Workload API default
// to what the settings file says.
func Command() *cobra.Command {
	var f flags
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Carry the lead's tasks to the agents, and their output, questions and messages back",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conf, err := config.FromCommand(cmd)
			if err != nil {
				return err
			}
			config.Unless(cmd, "trust-domain", &f.trustDomain, conf.TrustDomain)
			config.Unless(cmd, "workload-api", &f.workloadAPI, conf.WorkloadAPI)
			// A flag is held to what the file is held to.
			conf.TrustDomain, conf.WorkloadAPI = f.trustDomain, f.workloadAPI
			if err := conf.Validate(); err != nil {
				return err
			}
			return run(cmd.Context(), f, slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.socket, "socket", "/run/redoma-relay/relay.sock", "the agents' socket, which the host Envoy connects to over mutual TLS")
	fl.StringVar(&f.socketGroup, "socket-group", "redoma-relay", "the group of the agents' socket (the host Envoy joins it)")
	fl.StringVar(&f.leadSocket, "lead-socket", "/run/redoma-relay-lead/lead.sock", "the lead's socket (redoma msg, redoma mcp), in a directory of its own: not the agents' socket's, which the Envoy container mounts")
	fl.StringVar(&f.leadGroup, "lead-group", "redoma-lead", "the group of the lead's socket, the users who may task agents")
	fl.StringVar(&f.stateDir, "state-dir", "/var/lib/redoma-relay", "each agent's queue, its record and what the lead has not seen")
	fl.StringVar(&f.workloadAPI, "workload-api", "", "the SPIRE Workload API the relay's SVID comes from (default: the settings file's spire_workload_api)")
	fl.StringVar(&f.allowlist, "allowlist", "/etc/redoma/agents.txt", "the agent names that may connect, one per line")
	fl.StringVar(&f.trustDomain, "trust-domain", "", "the agents' SPIFFE trust domain (default: the settings file's trust_domain)")
	fl.DurationVar(&f.ackEvery, "ack-every", 200*time.Millisecond, "how often what an agent sent is saved and acked")
	return cmd
}

func run(ctx context.Context, f flags, log *slog.Logger) error {
	log.Info("redoma relay starting", "version", version.Version, "socket", f.socket, "lead_socket", f.leadSocket, "workload_api", f.workloadAPI)
	// The Envoy container mounts the agents' socket's directory: the lead's
	// socket is elsewhere.
	if sameDir(filepath.Dir(f.socket), filepath.Dir(f.leadSocket)) {
		return fmt.Errorf("--socket and --lead-socket are in one directory, %s: the lead's socket must not be where Envoy mounts", filepath.Dir(f.socket))
	}
	agentsGID, err := groupID(f.socketGroup)
	if err != nil {
		return err
	}
	leadGID, err := groupID(f.leadGroup)
	if err != nil {
		return err
	}
	if agentsGID == leadGID {
		return fmt.Errorf("--socket-group and --lead-group are both gid %d: the lead's users could reach the agents' socket", leadGID)
	}
	list := allowlist.File{Path: f.allowlist, Log: log}
	h, err := hub.New(hub.Config{Dir: f.stateDir, TrustDomain: f.trustDomain, Allowed: list.Allowed, AckEvery: f.ackEvery, Log: log})
	if err != nil {
		return err
	}
	defer h.Close()

	// Waits for the first SVID: until SPIRE has one, nothing is served.
	src, err := svid.Open(ctx, f.workloadAPI)
	if err != nil {
		return fmt.Errorf("the relay's SVID from %s: %w", f.workloadAPI, err)
	}
	defer src.Close()
	tlsCfg, err := mtls.ServerConfig(src, f.trustDomain, list.Allowed)
	if err != nil {
		return err
	}

	// Each directory 0750 in its socket's group: the Envoy container mounts
	// the agents' one, and only the lead's users traverse the other.
	for dir, gid := range map[string]int{filepath.Dir(f.socket): agentsGID, filepath.Dir(f.leadSocket): leadGID} {
		if err := setDir(dir, gid); err != nil {
			return err
		}
	}
	agents, err := listen(f.socket, agentsGID)
	if err != nil {
		return err
	}
	lead, err := listen(f.leadSocket, leadGID)
	if err != nil {
		agents.Close()
		return err
	}
	ag := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.KeepaliveParams(serverKeepalive),
		grpc.KeepaliveEnforcementPolicy(enforcement),
		// One envelope is small (hub.MaxRecvMsgSize), and a connection is
		// one agent's one stream: Envoy opens another for a newer one.
		grpc.MaxRecvMsgSize(hub.MaxRecvMsgSize),
		grpc.MaxConcurrentStreams(1),
	)
	relayv1.RegisterRelayServer(ag, h.RelayServer())
	ld := grpc.NewServer()
	relayv1.RegisterLeadServer(ld, h.LeadServer())
	// The one line between the start and the first agent: the SVID came
	// and both sockets are up. Until it, the relay waits for SPIRE.
	attrs := []any{"socket", f.socket, "lead_socket", f.leadSocket}
	if s, err := src.GetX509SVID(); err == nil {
		attrs = append(attrs, "svid", s.ID.String(), "svid_expires", s.Certificates[0].NotAfter)
	}
	log.Info("redoma relay serving", attrs...)
	return serve(ctx, map[*grpc.Server]net.Listener{ag: agents, ld: lead})
}

// sameDir reports whether a and b are one directory: the same file when
// both exist (/var/run/x is /run/x), else the same cleaned path.
func sameDir(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(fa, fb)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// groupID is the gid of a group name.
func groupID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// serve serves each server on its listener until ctx ends or one fails,
// then stops them all.
func serve(ctx context.Context, servers map[*grpc.Server]net.Listener) error {
	errc := make(chan error, len(servers))
	for g, l := range servers {
		go func() { errc <- g.Serve(l) }()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	for g := range servers {
		g.Stop()
	}
	return err
}

// listen opens a unix socket at path, replacing a stale one, and sets its
// owner (this process's uid), its group (gid) and SocketMode explicitly, so
// that nothing is inherited from the directory (a setgid directory would
// give it the directory's group). It then checks what the socket has.
func listen(path string, gid int) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Until the chmod the socket has the umask's mode, which the unit sets
	// to owner-only (UMask=0077).
	if err := setOwner(path, os.Getuid(), gid); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// DirMode is both socket directories' mode.
const DirMode = 0o750

// setDir gives a socket's directory (the relay's own, a RuntimeDirectory)
// the socket's group and DirMode, refusing anything but a real directory
// the relay owns.
func setDir(dir string, gid int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s must be a directory of the relay's own (its RuntimeDirectory)", dir)
	}
	if err := os.Chown(dir, -1, gid); err != nil {
		return fmt.Errorf("chgrp %s: %w", dir, err)
	}
	return os.Chmod(dir, DirMode)
}

func setOwner(path string, uid, gid int) error {
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	if err := os.Chmod(path, SocketMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner to check", path)
	}
	if int(st.Uid) != uid || int(st.Gid) != gid || fi.Mode().Perm() != SocketMode || fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is %v %d:%d after the chown, not a socket %o %d:%d", path, fi.Mode(), st.Uid, st.Gid, SocketMode, uid, gid)
	}
	return nil
}
