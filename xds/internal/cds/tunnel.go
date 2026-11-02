package cds

import (
	"strings"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TunnelName names the listener and the cluster of the tunnel to host:
// tunnel_<host with _ for .>.
func TunnelName(host string) string {
	return "tunnel_" + strings.ReplaceAll(host, ".", "_")
}

// TunnelCluster is the upstream of the tunnel to a Roles Anywhere endpoint
// (lds.TunnelListener): host:443 and nothing else, resolved by Envoy, with
// no TLS of Envoy's own, since redoma xds does it end to end through the
// tunnel. One per endpoint some agent's settings name.
func TunnelCluster(host string) *clusterv3.Cluster {
	name := TunnelName(host)
	return &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_LOGICAL_DNS},
		DnsLookupFamily:      clusterv3.Cluster_V4_PREFERRED,
		ConnectTimeout:       durationpb.New(5 * time.Second),
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
							Address:       host,
							PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 443},
						}}},
					}},
				}},
			}},
		},
	}
}
