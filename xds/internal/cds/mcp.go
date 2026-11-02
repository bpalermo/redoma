package cds

import (
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/durationpb"
)

// MCPClusterName is the name of an MCP upstream's cluster.
func MCPClusterName(upstream string) string {
	return "mcp_" + upstream
}

// MCPCluster is the upstream's server: port on 127.0.0.1 in the upstream's
// own namespace, netns, reached from a source address bound in that
// namespace (the bind config's network_namespace_filepath), plain HTTP. The
// server has no network but Envoy's, so nothing else reaches it. One per
// upstream, shared by every agent that has it.
func MCPCluster(upstream, netns string, port uint32) *clusterv3.Cluster {
	name := MCPClusterName(upstream)
	return &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(time.Second),
		UpstreamBindConfig: &corev3.BindConfig{SourceAddress: &corev3.SocketAddress{
			Address:                  "127.0.0.1",
			PortSpecifier:            &corev3.SocketAddress_PortValue{PortValue: 0},
			NetworkNamespaceFilepath: netns,
		}},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
							Address:       "127.0.0.1",
							PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
						}}},
					}},
				}},
			}},
		},
	}
}
