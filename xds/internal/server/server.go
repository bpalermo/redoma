// Package server serves the snapshot cache to the host Envoy over gRPC on a
// unix socket: ADS, and CDS, LDS and SDS on their own services for an Envoy that
// is configured without ADS.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	secretv3 "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	xdsserver "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SocketMode is the xDS socket's mode: its owner (uid 961) and its group,
// redoma-sds (gid 961). The host Envoy runs as root but with every
// capability dropped except CAP_SYS_ADMIN, so file permissions apply to it:
// its container joins group 961 to connect. Whoever connects is served every
// agent's private key, so nobody else may.
const SocketMode = 0o660

// Listen opens the unix socket at path, replacing a stale one left by an
// earlier run, with SocketMode.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Until the chmod the socket has the umask's mode, which the unit sets to
	// owner-only (UMask=0077), so it is never wider than SocketMode. The
	// directory (the unit's RuntimeDirectory, 0750) keeps others out too.
	if err := os.Chmod(path, SocketMode); err != nil {
		l.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return l, nil
}

// ListenerVersions are callbacks that report the listener version Envoy
// says it has applied: the version_info of each SotW LDS request, ACK or
// NACK (a NACK carries the last version applied). Delta streams report
// nothing.
func ListenerVersions(applied func(version string)) xdsserver.Callbacks {
	return xdsserver.CallbackFuncs{
		StreamRequestFunc: func(_ int64, req *discoveryv3.DiscoveryRequest) error {
			if req.GetTypeUrl() == resource.ListenerType && req.GetVersionInfo() != "" {
				applied(req.GetVersionInfo())
			}
			return nil
		},
	}
}

// sotwOnly refuses delta xDS. The control plane rebinds an agent whose
// namespace was replaced only after Envoy reports a listener version applied,
// and only SotW LDS requests carry one (ListenerVersions): under delta a
// waiting agent would never be bound again. The bootstrap uses
// api_type: GRPC; an Envoy configured for DELTA_GRPC fails here, loudly,
// rather than half-working.
type sotwOnly struct {
	xdsserver.Server
	log *slog.Logger
}

func (s sotwOnly) refuse(service string) error {
	s.log.Error("refusing a delta xDS stream: redoma-xds serves SotW only; set the bootstrap's ads_config api_type to GRPC", "service", service)
	return status.Error(codes.Unimplemented, "redoma-xds serves SotW xDS only (api_type: GRPC), not delta")
}

func (s sotwOnly) DeltaAggregatedResources(discoveryv3.AggregatedDiscoveryService_DeltaAggregatedResourcesServer) error {
	return s.refuse("ADS")
}

func (s sotwOnly) DeltaClusters(clusterv3.ClusterDiscoveryService_DeltaClustersServer) error {
	return s.refuse("CDS")
}

func (s sotwOnly) DeltaListeners(listenerv3.ListenerDiscoveryService_DeltaListenersServer) error {
	return s.refuse("LDS")
}

func (s sotwOnly) DeltaSecrets(secretv3.SecretDiscoveryService_DeltaSecretsServer) error {
	return s.refuse("SDS")
}

// Serve serves c on l until ctx ends, SotW only, calling cb on the xDS
// streams' events (nil for none).
func Serve(ctx context.Context, l net.Listener, c cache.Cache, cb xdsserver.Callbacks, log *slog.Logger) error {
	g := grpc.NewServer()
	xds := sotwOnly{Server: xdsserver.NewServer(ctx, c, cb), log: log}
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(g, xds)
	clusterv3.RegisterClusterDiscoveryServiceServer(g, xds)
	listenerv3.RegisterListenerDiscoveryServiceServer(g, xds)
	secretv3.RegisterSecretDiscoveryServiceServer(g, xds)

	errc := make(chan error, 1)
	go func() { errc <- g.Serve(l) }()
	select {
	case <-ctx.Done():
		g.Stop()
		<-errc
		return nil
	case err := <-errc:
		return err
	}
}
