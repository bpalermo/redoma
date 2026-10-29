package server

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/bpalermo/redoma/xds/internal/controlplane"
	"github.com/bpalermo/redoma/xds/internal/identity/identitytest"
	"github.com/bpalermo/redoma/xds/internal/lds"
	"github.com/bpalermo/redoma/xds/internal/modelauth"
)

// serve serves a snapshot holding agent a's listeners on l until the test
// ends, and returns the listener versions the client reports applied.
func serve(t *testing.T, l net.Listener) <-chan string {
	t.Helper()
	c := cache.NewSnapshotCache(true, controlplane.NodeHash{}, nil)
	var listeners []types.Resource
	for _, l := range lds.Listeners("/run/netns/redoma-a", "a", modelauth.APIKey, nil) {
		listeners = append(listeners, l)
	}
	snap, err := cache.NewSnapshot("1", map[resource.Type][]types.Resource{resource.ListenerType: listeners})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetSnapshot(t.Context(), controlplane.NodeKey, snap); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	applied := make(chan string, 16)
	go func() {
		done <- Serve(ctx, l, c, ListenerVersions(func(v string) { applied <- v }), slog.New(slog.DiscardHandler))
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return applied
}

// fetchListeners asks for the listeners over ADS, as the host Envoy's
// bootstrap will, and returns their names, sorted.
func fetchListeners(t *testing.T, conn *grpc.ClientConn) []string {
	t.Helper()
	stream, err := discoveryv3.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		Node:    &corev3.Node{Id: "redoma-host", Cluster: "redoma"},
		TypeUrl: resource.ListenerType,
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetVersionInfo() != "1" {
		t.Errorf("version %q, want 1", resp.GetVersionInfo())
	}
	// The ACK, as Envoy sends it once the listeners are applied.
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		VersionInfo:   resp.GetVersionInfo(),
		ResponseNonce: resp.GetNonce(),
		TypeUrl:       resource.ListenerType,
	}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range resp.GetResources() {
		l := &listenerv3.Listener{}
		if err := r.UnmarshalTo(l); err != nil {
			t.Fatal(err)
		}
		names = append(names, l.GetName())
	}
	slices.Sort(names)
	return names
}

var wantListeners = []string{"agent_a_egress", "agent_a_model"}

func TestServesListenersOverADS(t *testing.T) {
	l := bufconn.Listen(1 << 20)
	applied := serve(t, l)
	conn, err := grpc.NewClient("passthrough:///xds",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := fetchListeners(t, conn); !slices.Equal(got, wantListeners) {
		t.Errorf("listeners %q, want %q", got, wantListeners)
	}
	// The first request carries no version; the ACK carries "1".
	select {
	case v := <-applied:
		if v != "1" {
			t.Errorf("applied version %q, want 1", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the ACK was not reported")
	}
}

// TestRefusesDelta: a delta stream, on ADS, CDS, LDS or SDS, is refused as
// Unimplemented, since the rebind of a replaced namespace needs SotW reports.
func TestRefusesDelta(t *testing.T) {
	l := bufconn.Listen(1 << 20)
	serve(t, l)
	conn, err := grpc.NewClient("passthrough:///xds",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := &discoveryv3.DeltaDiscoveryRequest{Node: &corev3.Node{Id: "redoma-host"}, TypeUrl: resource.ListenerType}

	ads, err := discoveryv3.NewAggregatedDiscoveryServiceClient(conn).DeltaAggregatedResources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = ads.Send(req)
	if _, err := ads.Recv(); status.Code(err) != codes.Unimplemented {
		t.Errorf("delta ADS: %v, want Unimplemented", err)
	}
	lds, err := listenerservice.NewListenerDiscoveryServiceClient(conn).DeltaListeners(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = lds.Send(req)
	if _, err := lds.Recv(); status.Code(err) != codes.Unimplemented {
		t.Errorf("delta LDS: %v, want Unimplemented", err)
	}
	sds, err := secretservice.NewSecretDiscoveryServiceClient(conn).DeltaSecrets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = sds.Send(&discoveryv3.DeltaDiscoveryRequest{Node: &corev3.Node{Id: "redoma-host"}, TypeUrl: resource.SecretType})
	if _, err := sds.Recv(); status.Code(err) != codes.Unimplemented {
		t.Errorf("delta SDS: %v, want Unimplemented", err)
	}
	cds, err := clusterservice.NewClusterDiscoveryServiceClient(conn).DeltaClusters(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = cds.Send(&discoveryv3.DeltaDiscoveryRequest{Node: &corev3.Node{Id: "redoma-host"}, TypeUrl: resource.ClusterType})
	if _, err := cds.Recv(); status.Code(err) != codes.Unimplemented {
		t.Errorf("delta CDS: %v, want Unimplemented", err)
	}
}

// TestUnixSocketsRefused: the two socket tests skip on a refused socket(2)
// only, and fail on anything else.
func TestUnixSocketsRefused(t *testing.T) {
	refused := &net.OpError{Op: "listen", Net: "unix", Err: os.NewSyscallError("socket", syscall.EPERM)}
	if !identitytest.UnixSocketsRefused(refused) {
		t.Error("a refused socket(2) is not recognised")
	}
	missing := &net.OpError{Op: "listen", Net: "unix", Err: os.NewSyscallError("bind", syscall.ENOENT)}
	if identitytest.UnixSocketsRefused(missing) {
		t.Error("a bind ENOENT would skip the test")
	}
	denied := &net.OpError{Op: "listen", Net: "unix", Err: os.NewSyscallError("bind", syscall.EACCES)}
	if identitytest.UnixSocketsRefused(denied) {
		t.Error("a bind EACCES would skip the test")
	}
}

// TestListen: the socket replaces a stale one, has SocketMode, and serves ADS.
// Skipped where the environment forbids unix sockets.
func TestListen(t *testing.T) {
	// A socket path is limited to 107 bytes, which Bazel's test temporary
	// directory can exceed.
	dir, err := os.MkdirTemp("/tmp", "redoma-")
	if err != nil {
		dir = t.TempDir()
	} else {
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
	path := filepath.Join(dir, "xds.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(path)
	identitytest.SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != SocketMode {
		t.Errorf("socket mode %v, want a %o socket", fi.Mode(), SocketMode)
	}
	serve(t, l)
	conn, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := fetchListeners(t, conn); !slices.Equal(got, wantListeners) {
		t.Errorf("listeners %q, want %q", got, wantListeners)
	}
}
