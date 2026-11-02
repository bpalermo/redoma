package lds

import (
	"time"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bpalermo/redoma/xds/internal/cds"
)

// DefaultAWSPort is the agent-side port of the AWS credentials listener,
// unless redoma xds is told another (--aws-credentials-port).
const DefaultAWSPort = 9911

// awsPath is the one path the listener forwards: the AWS SDKs' container
// credentials provider's (awscreds.CredentialsPath).
const awsPath = "/v1/credentials"

// awsTimeout bounds a request: redoma xds answers from memory, or within
// its own wait for a session in flight, well under it.
const awsTimeout = 5 * time.Second

// AWSListener is the agent's AWS credentials listener, 127.0.0.1:port in its
// namespace, for an agent with AWS settings only: GET /v1/credentials,
// HTTP/1.1, forwarded to the agent's own cluster (cds.AWSCluster), redoma
// xds's socket for that agent alone, which is how redoma xds knows the agent.
// Any other path is a 404 here, any other method a 404 from redoma xds.
func AWSListener(netns, repo string, port uint32) *listenerv3.Listener {
	routes := &routev3.RouteConfiguration{
		// The cluster comes in the same snapshot, but a listener must never
		// be refused for it.
		ValidateClusters: wrapperspb.Bool(false),
		VirtualHosts: []*routev3.VirtualHost{{
			Name:    "aws",
			Domains: []string{"*"},
			Routes: []*routev3.Route{
				{
					Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: awsPath}},
					Action: &routev3.Route_Route{Route: &routev3.RouteAction{
						ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: cds.AWSClusterName(repo)},
						Timeout:          durationpb.New(awsTimeout),
					}},
				},
				{
					Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
					Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 404}},
				},
			},
		}},
	}
	cfg := &hcmv3.HttpConnectionManager{
		CodecType:      hcmv3.HttpConnectionManager_HTTP1,
		StatPrefix:     ListenerName(repo, "aws"),
		AccessLog:      accessLog(repo, "aws"),
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: routes},
		HttpFilters:    []*hcmv3.HttpFilter{httpFilter("envoy.filters.http.router", &routerv3.Router{})},
	}
	return &listenerv3.Listener{
		Name:    ListenerName(repo, "aws"),
		Address: address(netns, port),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(cfg)},
			}},
		}},
	}
}
