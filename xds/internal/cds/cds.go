// Package cds builds the host Envoy's per-agent clusters, served over ADS
// beside the listeners. The bootstrap's own clusters (anthropic,
// egress_dfp: examples/envoy.yaml) are not here.
//
// One per running agent, agent_<repo>_relay: the upstream of the agent's
// relay listener (lds.RelayListener), redoma-relay's unix socket, over
// mutual TLS. The client certificate is the agent's own SVID (the SDS secret
// agent/<repo>), so the relay knows the agent from the connection alone and
// the container never holds the key. One cluster per agent because a
// cluster has one client certificate; and a cluster of its own is never
// rebuilt, its stream drained, when another agent comes or goes.
package cds

import (
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Keepalive of the relay's long-lived stream, on both of its hops (the
// listener's too): HTTP/2 pings every RelayPing, a connection dead after
// RelayPingTimeout more without an answer, and closed after
// RelayConnectionIdle with no stream.
const (
	RelayPing           = 30 * time.Second
	RelayPingTimeout    = 10 * time.Second
	RelayConnectionIdle = time.Hour
)

// RelayKeepalive is the HTTP/2 keepalive of both hops.
func RelayKeepalive() *corev3.KeepaliveSettings {
	return &corev3.KeepaliveSettings{
		Interval: durationpb.New(RelayPing),
		Timeout:  durationpb.New(RelayPingTimeout),
	}
}

// RelayClusterName is the name of the agent's relay cluster.
func RelayClusterName(repo string) string {
	return "agent_" + repo + "_relay"
}

// Relay is where redoma-relay is and who it is.
type Relay struct {
	// Socket is its unix socket (/run/redoma-relay/relay.sock), as Envoy
	// sees it.
	Socket string
	// ID is its SPIFFE ID, which its certificate must carry.
	ID string
}

// RelayCluster is the agent's relay cluster. secret is the SDS name of the
// agent's SVID, bundle that of the trust domain's bundle; both come over ADS.
func RelayCluster(repo string, relay Relay, secret, bundle string) *clusterv3.Cluster {
	name := RelayClusterName(repo)
	ads := &corev3.ConfigSource{
		ResourceApiVersion:    corev3.ApiVersion_V3,
		ConfigSourceSpecifier: &corev3.ConfigSource_Ads{Ads: &corev3.AggregatedConfigSource{}},
	}
	tls := &tlsv3.UpstreamTlsContext{CommonTlsContext: &tlsv3.CommonTlsContext{
		// redoma-relay speaks TLS 1.3 only.
		TlsParams: &tlsv3.TlsParameters{
			TlsMinimumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
			TlsMaximumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
		},
		AlpnProtocols:                  []string{"h2"},
		TlsCertificateSdsSecretConfigs: []*tlsv3.SdsSecretConfig{{Name: secret, SdsConfig: ads}},
		// The relay's certificate chains to the bundle and carries the
		// relay's ID: any other workload of the trust domain is refused.
		ValidationContextType: &tlsv3.CommonTlsContext_CombinedValidationContext{
			CombinedValidationContext: &tlsv3.CommonTlsContext_CombinedCertificateValidationContext{
				DefaultValidationContext: &tlsv3.CertificateValidationContext{
					MatchTypedSubjectAltNames: []*tlsv3.SubjectAltNameMatcher{{
						SanType: tlsv3.SubjectAltNameMatcher_URI,
						Matcher: &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: relay.ID}},
					}},
				},
				ValidationContextSdsSecretConfig: &tlsv3.SdsSecretConfig{Name: bundle, SdsConfig: ads},
			},
		},
	}}
	return &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(time.Second),
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{Address: &corev3.Address_Pipe{Pipe: &corev3.Pipe{Path: relay.Socket}}},
					}},
				}},
			}},
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(&httpv3.HttpProtocolOptions{
				CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{IdleTimeout: durationpb.New(RelayConnectionIdle)},
				UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
					ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
						Http2ProtocolOptions: &corev3.Http2ProtocolOptions{ConnectionKeepalive: RelayKeepalive()},
					},
				}},
			}),
		},
		TransportSocket: &corev3.TransportSocket{
			Name:       "envoy.transport_sockets.tls",
			ConfigType: &corev3.TransportSocket_TypedConfig{TypedConfig: mustAny(tls)},
		},
	}
}

// mustAny packs a message that is always valid, deterministically (see
// lds.mustAny: a SotW update sends every cluster again, so an unchanged one
// must be the same bytes).
func mustAny(m proto.Message) *anypb.Any {
	a := &anypb.Any{}
	if err := anypb.MarshalFrom(a, m, proto.MarshalOptions{Deterministic: true}); err != nil {
		panic(err)
	}
	return a
}
