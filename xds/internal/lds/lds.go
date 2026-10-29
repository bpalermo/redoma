// Package lds builds the host Envoy's listeners for one running agent.
//
// One agent is one network namespace, whose path the agent.Source gives. Each
// gets two listeners bound INSIDE that namespace
// (SocketAddress.network_namespace_filepath), so a connection's listener is
// its agent's identity: a container can only open sockets in its own
// namespace.
//
//	127.0.0.1:8080  model: Claude's API. Envoy replaces whatever credential
//	                the agent sent with the real one (credential_injector, an
//	                SDS file), in the header the modelauth.Mode names, then
//	                forwards /v1/messages and /v1/messages/count_tokens to
//	                api.anthropic.com; any other path gets a 403.
//	127.0.0.1:3128  egress: an HTTP CONNECT proxy that opens tunnels only to
//	                the agent's allowed hosts; everything else gets a 403.
//
// lds_test.go holds them to the goldens in testdata.
package lds

import (
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/stream/v3"
	commondfpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/common/dynamic_forward_proxy/v3"
	credinjectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/credential_injector/v3"
	dfpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_forward_proxy/v3"
	ratelimitv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/local_ratelimit/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	genericv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/http/injected_credentials/generic/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bpalermo/redoma/xds/internal/modelauth"
)

const (
	// ModelPort and EgressPort are the agent-side ports, on 127.0.0.1 of the
	// agent's namespace.
	ModelPort  = 8080
	EgressPort = 3128

	// The clusters of the host Envoy's bootstrap (examples/envoy.yaml).
	anthropicCluster = "anthropic"
	egressCluster    = "egress_dfp"

	// The model API key, an SDS file only root can read; the key never enters
	// a container and never passes through this control plane.
	anthropicKeySecret = "anthropic-key"
	anthropicKeyPath   = "/etc/redoma/secrets/anthropic-key.yaml"
)

// modelPaths are the paths the model listener forwards: Claude Code's
// inference, the Messages API and its token count. Any other path of
// api.anthropic.com (the Admin API, files, batches, an OAuth profile) is
// refused, so the credential is never spent on it.
var modelPaths = []string{"/v1/messages", "/v1/messages/count_tokens"}

// ListenerName is the name of an agent's listener of the given kind ("model"
// or "egress"), also its stats prefix.
func ListenerName(repo, kind string) string {
	return "agent_" + repo + "_" + kind
}

// Listeners returns the agent's two listeners, model then egress, bound in the
// namespace at netns. auth is how the model listener injects the credential;
// hosts are the host names it may open CONNECT tunnels to, on port 443.
func Listeners(netns, repo string, auth modelauth.Mode, hosts []string) []*listenerv3.Listener {
	return []*listenerv3.Listener{
		modelListener(netns, repo, auth),
		egressListener(netns, repo, hosts),
	}
}

func address(netns string, port uint32) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
		Address:                  "127.0.0.1",
		PortSpecifier:            &corev3.SocketAddress_PortValue{PortValue: port},
		NetworkNamespaceFilepath: netns,
	}}}
}

// mustAny packs a message that is always valid: Any only fails to marshal a
// message proto cannot encode. Deterministically: the access log's format is
// a map, and in a SotW update every listener is sent again when any agent
// changes, so the same listener must be the same bytes or it would look
// changed to Envoy.
func mustAny(m proto.Message) *anypb.Any {
	a := &anypb.Any{}
	if err := anypb.MarshalFrom(a, m, proto.MarshalOptions{Deterministic: true}); err != nil {
		panic(err)
	}
	return a
}

func accessLog(repo, kind string) []*accesslogv3.AccessLog {
	fields := map[string]string{
		"agent": repo, "listener": kind, "start": "%START_TIME%",
		"method": "%REQ(:METHOD)%", "authority": "%REQ(:AUTHORITY)%", "path": "%REQ(:PATH)%",
		"code": "%RESPONSE_CODE%", "flags": "%RESPONSE_FLAGS%", "duration_ms": "%DURATION%",
		"bytes_in": "%BYTES_RECEIVED%", "bytes_out": "%BYTES_SENT%",
	}
	format := &structpb.Struct{Fields: map[string]*structpb.Value{}}
	for k, v := range fields {
		format.Fields[k] = structpb.NewStringValue(v)
	}
	return []*accesslogv3.AccessLog{{
		Name: "envoy.access_loggers.stdout",
		ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: mustAny(&streamv3.StdoutAccessLog{
			AccessLogFormat: &streamv3.StdoutAccessLog_LogFormat{LogFormat: &corev3.SubstitutionFormatString{
				Format: &corev3.SubstitutionFormatString_JsonFormat{JsonFormat: format},
			}},
		})},
	}}
}

func httpFilter(name string, config proto.Message) *hcmv3.HttpFilter {
	return &hcmv3.HttpFilter{Name: name, ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: mustAny(config)}}
}

// hcm is the listener's one network filter; the router is appended to filters.
func hcm(repo, kind string, routes *routev3.RouteConfiguration, filters []*hcmv3.HttpFilter, idle *durationpb.Duration) *listenerv3.Filter {
	cfg := &hcmv3.HttpConnectionManager{
		StatPrefix:     ListenerName(repo, kind),
		AccessLog:      accessLog(repo, kind),
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: routes},
		HttpFilters:    append(filters, httpFilter("envoy.filters.http.router", &routerv3.Router{})),
	}
	cfg.StreamIdleTimeout = idle
	return &listenerv3.Filter{
		Name:       "envoy.filters.network.http_connection_manager",
		ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(cfg)},
	}
}

func percentAll() *corev3.RuntimeFractionalPercent {
	return &corev3.RuntimeFractionalPercent{DefaultValue: &typev3.FractionalPercent{
		Numerator: 100, Denominator: typev3.FractionalPercent_HUNDRED,
	}}
}

// credential is the model listener's credential_injector for auth, if any,
// and the headers its route strips: the credential header auth does not
// write. Only the header's value is replaced (overwrite); nothing else in the
// request is added or changed. None has no injector and strips both, so the
// request carries no credential at all.
func credential(auth modelauth.Mode) (*hcmv3.HttpFilter, []string) {
	var header, strip string
	switch auth {
	case modelauth.APIKey:
		header, strip = "x-api-key", "authorization"
	case modelauth.Bearer:
		header, strip = "authorization", "x-api-key"
	default:
		return nil, []string{"authorization", "x-api-key"}
	}
	return httpFilter("envoy.filters.http.credential_injector", &credinjectorv3.CredentialInjector{
		Overwrite: true,
		Credential: &corev3.TypedExtensionConfig{
			Name: "envoy.http.injected_credentials.generic",
			TypedConfig: mustAny(&genericv3.Generic{
				Credential: &tlsv3.SdsSecretConfig{
					Name: anthropicKeySecret,
					SdsConfig: &corev3.ConfigSource{ConfigSourceSpecifier: &corev3.ConfigSource_PathConfigSource{
						PathConfigSource: &corev3.PathConfigSource{Path: anthropicKeyPath},
					}},
				},
				Header: header,
			}),
		},
	}), []string{strip}
}

func modelListener(netns, repo string, auth modelauth.Mode) *listenerv3.Listener {
	injector, strip := credential(auth)
	// Only inference is forwarded, and so only inference carries the
	// credential: every other path is a local 403 that never leaves Envoy.
	var routeList []*routev3.Route
	for _, path := range modelPaths {
		routeList = append(routeList, &routev3.Route{
			// An exact path, matched without the query string (Claude Code
			// sends ?beta=true).
			Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: path}},
			// A model response streams for minutes: no route timeout; the HCM's
			// stream idle timeout ends a stalled one.
			Action: &routev3.Route_Route{Route: &routev3.RouteAction{
				ClusterSpecifier:     &routev3.RouteAction_Cluster{Cluster: anthropicCluster},
				Timeout:              durationpb.New(0),
				HostRewriteSpecifier: &routev3.RouteAction_HostRewriteLiteral{HostRewriteLiteral: "api.anthropic.com"},
			}},
			RequestHeadersToRemove: strip,
		})
	}
	routeList = append(routeList, &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: forbidden(),
	})
	routes := &routev3.RouteConfiguration{VirtualHosts: []*routev3.VirtualHost{{
		Name:    "anthropic",
		Domains: []string{"*"},
		Routes:  routeList,
	}}}
	filters := []*hcmv3.HttpFilter{
		httpFilter("envoy.filters.http.local_ratelimit", &ratelimitv3.LocalRateLimit{
			StatPrefix: ListenerName(repo, "model"),
			TokenBucket: &typev3.TokenBucket{
				MaxTokens:     60,
				TokensPerFill: wrapperspb.UInt32(60),
				FillInterval:  durationpb.New(60 * time.Second),
			},
			FilterEnabled:  percentAll(),
			FilterEnforced: percentAll(),
		}),
	}
	if injector != nil {
		filters = append(filters, injector)
	}
	return &listenerv3.Listener{
		Name:    ListenerName(repo, "model"),
		Address: address(netns, ModelPort),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{hcm(repo, "model", routes, filters, durationpb.New(600*time.Second))},
		}},
	}
}

func egressListener(netns, repo string, hosts []string) *listenerv3.Listener {
	var vhosts []*routev3.VirtualHost
	if len(hosts) > 0 {
		domains := make([]string, len(hosts))
		for i, h := range hosts {
			domains[i] = h + ":443"
		}
		vhosts = append(vhosts, &routev3.VirtualHost{
			Name:    "allowed",
			Domains: domains,
			Routes: []*routev3.Route{{
				Match: connectMatch(),
				Action: &routev3.Route_Route{Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: egressCluster},
					Timeout:          durationpb.New(0),
					UpgradeConfigs: []*routev3.RouteAction_UpgradeConfig{{
						UpgradeType:   "CONNECT",
						ConnectConfig: &routev3.RouteAction_UpgradeConfig_ConnectConfig{},
					}},
				}},
			}},
		})
	}
	vhosts = append(vhosts, &routev3.VirtualHost{
		Name:    "denied",
		Domains: []string{"*"},
		Routes: []*routev3.Route{
			{Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}}, Action: forbidden()},
			{Match: connectMatch(), Action: forbidden()},
		},
	})
	filters := []*hcmv3.HttpFilter{
		httpFilter("envoy.filters.http.dynamic_forward_proxy", &dfpv3.FilterConfig{
			ImplementationSpecifier: &dfpv3.FilterConfig_DnsCacheConfig{DnsCacheConfig: &commondfpv3.DnsCacheConfig{
				Name:            "egress",
				DnsLookupFamily: clusterv3.Cluster_V4_PREFERRED,
			}},
		}),
	}
	return &listenerv3.Listener{
		Name:    ListenerName(repo, "egress"),
		Address: address(netns, EgressPort),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{hcm(repo, "egress", &routev3.RouteConfiguration{VirtualHosts: vhosts}, filters, nil)},
		}},
	}
}

func connectMatch() *routev3.RouteMatch {
	return &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_ConnectMatcher_{ConnectMatcher: &routev3.RouteMatch_ConnectMatcher{}}}
}

func forbidden() *routev3.Route_DirectResponse {
	return &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 403}}
}
