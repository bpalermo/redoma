package lds

import (
	"path/filepath"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbacconfigv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	mcpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/mcp/v3"
	rbacv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bpalermo/redoma/xds/internal/mcpconf"
)

// CheckSocketMode is the mode of a tool check listener's socket. Envoy,
// root with only its supplementary groups' rights, creates it and cannot
// give it redoma xds's group, as a tunnel's: the directory (0770,
// redoma-sds, made by redoma xds) is what keeps everyone but Envoy and
// redoma xds out.
const CheckSocketMode = TunnelSocketMode

// checkMethods are what the tool check POSTs: the session's start and
// tools/list, nothing else; a tools/call through it is refused. It ends
// the session with a DELETE.
var checkMethods = []string{"initialize", "notifications/initialized", "ping", "tools/list"}

// CheckListenerName names upstream's tool check listener.
func CheckListenerName(upstream string) string {
	return "upstream_" + upstream + "_check"
}

// CheckSocket is upstream's tool check socket in dir.
func CheckSocket(dir, upstream string) string {
	return filepath.Join(dir, "mcp-check-"+upstream+".sock")
}

// CheckListener is the listener redoma xds asks upstream for its tools
// through (docs/mcp-upstreams.md, "The tool check"): a unix socket in the
// host's namespace, at socket, whose requests to MCPPath(upstream) go to
// the upstream's cluster as an agent's would, through the MCP filter
// (REJECT_NO_MCP, BODY, duplicate keys refused) and an RBAC policy that
// allows the session's start and tools/list alone. No agent reaches it.
func CheckListener(u *mcpconf.Upstream, socket string) *listenerv3.Listener {
	name := CheckListenerName(u.Name)
	var methods []*rbacconfigv3.Permission
	for _, m := range checkMethods {
		methods = append(methods, metadataPerm(m, "method"))
	}
	rules := &rbacconfigv3.RBAC{Action: rbacconfigv3.RBAC_ALLOW, Policies: map[string]*rbacconfigv3.Policy{
		"tool-check": {
			Permissions: []*rbacconfigv3.Permission{andPerm(pathPerm(MCPPath(u.Name)), orPerm(
				andPerm(methodPerm("POST"), orPerm(methods...)),
				// The check's end of its session. Only redoma xds reaches
				// this listener, so a body on it is no agent's.
				methodPerm("DELETE"),
			))},
			Principals: []*rbacconfigv3.Principal{{Identifier: &rbacconfigv3.Principal_Any{Any: true}}},
		},
	}}
	// The server's whole static ceiling: none of the per-agent headers an
	// agent's route adds (an X-MCP-Tools would narrow the list).
	route := upstreamRoute(&routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: MCPPath(u.Name)}}, u, "")
	route.RequestHeadersToAdd = nil
	cfg := &hcmv3.HttpConnectionManager{
		StatPrefix: name,
		AccessLog:  accessLogFields(map[string]string{"upstream": u.Name, "listener": "check", "mcp_method": "%DYNAMIC_METADATA(" + mcpFilter + ":method)%"}),
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: &routev3.RouteConfiguration{VirtualHosts: []*routev3.VirtualHost{{
			Name:    "check",
			Domains: []string{"*"},
			Routes: []*routev3.Route{
				route,
				{
					Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
					Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 404}},
				},
			},
		}}}},
		HttpFilters: []*hcmv3.HttpFilter{
			httpFilter(mcpFilter, &mcpv3.Mcp{
				TrafficMode:         mcpv3.Mcp_REJECT_NO_MCP,
				AttributeSource:     mcpv3.Mcp_BODY,
				RejectDuplicateKeys: wrapperspb.Bool(true),
				MaxRequestBodySize:  wrapperspb.UInt32(mcpconf.DefaultMaxRequestBytes),
			}),
			httpFilter("envoy.filters.http.rbac", &rbacv3.RBAC{Rules: rules}),
			httpFilter("envoy.filters.http.router", &routerv3.Router{}),
		},
		NormalizePath:                wrapperspb.Bool(true),
		MergeSlashes:                 true,
		PathWithEscapedSlashesAction: hcmv3.HttpConnectionManager_REJECT_REQUEST,
	}
	return &listenerv3.Listener{
		Name:         name,
		Address:      &corev3.Address{Address: &corev3.Address_Pipe{Pipe: &corev3.Pipe{Path: socket, Mode: CheckSocketMode}}},
		FilterChains: []*listenerv3.FilterChain{{Filters: []*listenerv3.Filter{hcmFilter(cfg)}}},
	}
}
