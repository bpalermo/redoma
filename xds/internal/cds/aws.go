package cds

import (
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/durationpb"
)

// AWSClusterName is the name of the agent's AWS credentials cluster.
func AWSClusterName(repo string) string {
	return "agent_" + repo + "_aws"
}

// AWSCluster is the upstream of the agent's AWS credentials listener
// (lds.AWSListener): redoma xds's unix socket for this agent alone
// (awscreds.Service.Socket), plain HTTP/1.1. The socket is the agent's
// identity, so the cluster is the agent's own, never shared.
func AWSCluster(repo, socket string) *clusterv3.Cluster {
	name := AWSClusterName(repo)
	return &clusterv3.Cluster{
		Name:                 name,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(time.Second),
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{Address: &corev3.Address_Pipe{Pipe: &corev3.Pipe{Path: socket}}},
					}},
				}},
			}},
		},
	}
}
