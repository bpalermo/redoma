// Command redoma-shim is an agent container's entrypoint. It holds the
// agent's one stream to redoma-relay (through the host Envoy's relay
// listener, 127.0.0.1:7070 in the container's namespace), runs the lead's
// tasks one at a time as claude turns, resuming one session, and streams
// their output back.
//
//	redoma-shim [flags]    the daemon
//	redoma-shim mcp        the MCP server claude runs for each turn (ask_lead, send_message)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/common/version"
	"github.com/bpalermo/redoma/shim/internal/bridge"
	"github.com/bpalermo/redoma/shim/internal/mcp"
	"github.com/bpalermo/redoma/shim/internal/reaper"
	"github.com/bpalermo/redoma/shim/internal/shim"
)

// list is a repeatable string flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, " ") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		srv := &mcp.Server{Lead: mcp.Remote(os.Getenv(bridge.EnvAddr)), Version: version.Version}
		if err := srv.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "redoma-shim mcp:", err)
			os.Exit(1)
		}
		return
	}
	var cfg shim.Config
	var claudeArgs list
	showVersion := flag.Bool("version", false, "print the version and exit")
	relay := flag.String("relay", "127.0.0.1:7070", "the relay listener (the host Envoy's, in this namespace)")
	flag.StringVar(&cfg.StateDir, "state-dir", "/work/.redoma", "the shim's state: its queues, the tasks and the session; keep it in the workspace")
	flag.StringVar(&cfg.Workdir, "workdir", "/work", "where claude runs")
	flag.StringVar(&cfg.Claude, "claude", "claude", "the claude binary")
	flag.Var(&claudeArgs, "claude-arg", "one more argument for every claude turn (repeatable)")
	flag.DurationVar(&cfg.AckEvery, "ack-every", 200*time.Millisecond, "how often what the relay sent is saved and acked")
	flag.DurationVar(&cfg.HeartbeatEvery, "heartbeat", 30*time.Second, "how often a heartbeat goes to the relay")
	flag.DurationVar(&cfg.MinBackoff, "min-backoff", 500*time.Millisecond, "the first wait before a reconnect")
	flag.DurationVar(&cfg.MaxBackoff, "max-backoff", 30*time.Second, "the longest wait before a reconnect")
	flag.DurationVar(&cfg.KillGrace, "kill-grace", 10*time.Second, "how long a cancelled turn has between SIGTERM and SIGKILL")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	cfg.ClaudeArgs = claudeArgs
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg.Log = log
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, *relay); err != nil {
		log.Error("redoma-shim failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg shim.Config, relay string) error {
	cfg.Log.Info("redoma-shim starting", "version", version.Version, "relay", relay, "state_dir", cfg.StateDir)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cfg.Self = self
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	l, err := listenBridge(filepath.Join(cfg.StateDir, "shim.sock"))
	if err != nil {
		return err
	}
	cfg.Bridge = l
	conn, err := grpc.NewClient(relay,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// Pings on the hop to Envoy, like Envoy's own on the next.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	cfg.Relay = relayv1.NewRelayClient(conn)
	// PID 1 of the container: it reaps what the turns leave (reaper).
	if cfg.Reaper, err = reaper.New(ctx, cfg.Log); err != nil {
		return err
	}
	s, err := shim.New(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Run(ctx)
}

// listenBridge opens the MCP bridge's socket, owner-only, replacing a stale
// one.
func listenBridge(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}
