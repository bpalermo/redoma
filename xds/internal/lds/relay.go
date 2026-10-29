package lds

import (
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bpalermo/redoma/xds/internal/cds"
)

// RelayPort is the agent-side port of the relay listener.
const RelayPort = 7070

// relayPath is the one gRPC method the relay listener forwards: the agent's
// stream (redoma api/relay/v1, Relay.Connect).
const relayPath = "/redoma.relay.v1.Relay/Connect"

// relayStreamIdle is how long the relay's stream may idle before Envoy ends
// it: the shim sends a heartbeat every 30s, and the pings (cds.RelayPing)
// are well within it.
const relayStreamIdle = 5 * time.Minute

// RelayListener is the agent's relay listener, 127.0.0.1:7070 in its
// namespace: the shim's one gRPC stream, HTTP/2 without TLS, forwarded to
// the agent's relay cluster (cds.RelayCluster), which carries it to
// redoma-relay over mutual TLS with the agent's SVID. Any other path is a
// 403. The stream has no route timeout and no maximum duration; only the
// stream idle timeout ends one that stopped moving.
func RelayListener(netns, repo string) *listenerv3.Listener {
	routes := &routev3.RouteConfiguration{
		// The cluster is CDS's, published once the agent's SVID is: a
		// listener must not be refused for it meanwhile (a request then
		// fails, as it should).
		ValidateClusters: wrapperspb.Bool(false),
		VirtualHosts: []*routev3.VirtualHost{{
			Name:    "relay",
			Domains: []string{"*"},
			Routes: []*routev3.Route{
				{
					Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: relayPath}},
					Action: &routev3.Route_Route{Route: &routev3.RouteAction{
						ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cds.RelayClusterName(repo)},
						Timeout:          durationpb.New(0),
					}},
				},
				{Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}}, Action: forbidden()},
			},
		}},
	}
	cfg := &hcmv3.HttpConnectionManager{
		CodecType:      hcmv3.HttpConnectionManager_HTTP2,
		StatPrefix:     ListenerName(repo, "relay"),
		AccessLog:      accessLog(repo, "relay"),
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: routes},
		HttpFilters:    []*hcmv3.HttpFilter{httpFilter("envoy.filters.http.router", &routerv3.Router{})},
		// The same pings and connection idle timeout as the cluster's hop.
		Http2ProtocolOptions:      &corev3.Http2ProtocolOptions{ConnectionKeepalive: cds.RelayKeepalive()},
		CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{IdleTimeout: durationpb.New(cds.RelayConnectionIdle)},
		StreamIdleTimeout:         durationpb.New(relayStreamIdle),
	}
	return &listenerv3.Listener{
		Name:    ListenerName(repo, "relay"),
		Address: address(netns, RelayPort),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(cfg)},
			}},
		}},
	}
}
