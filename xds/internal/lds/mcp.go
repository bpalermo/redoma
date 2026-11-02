package lds

import (
	"slices"
	"strconv"
	"strings"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	mutationv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbacconfigv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	headermutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	mcpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/mcp/v3"
	rbacv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bpalermo/redoma/xds/internal/cds"
	"github.com/bpalermo/redoma/xds/internal/mcpconf"
)

// MCPPort is the agent-side port of the MCP listener, on 127.0.0.1 of the
// agent's namespace.
const MCPPort = 8081

// mcpFilter is the MCP filter's name, also its dynamic metadata namespace.
const mcpFilter = "envoy.filters.http.mcp"

// lifecycle are the methods every agent with an upstream may call: the
// session's, and tools/list. Anything but these and an allowed tools/call
// is refused.
var lifecycle = []string{"initialize", "notifications/cancelled", "notifications/initialized", "ping", "tools/list"}

// MCPPath is where an agent reaches upstream: http://127.0.0.1:8081/mcp/<upstream>.
func MCPPath(upstream string) string {
	return "/mcp/" + upstream
}

// MCPRoute is one upstream an agent has.
type MCPRoute struct {
	Upstream *mcpconf.Upstream
	// Allowlist is the agent's tools for it. Nil when the agent's file (or
	// the defaults') is invalid: the route stays, and RBAC allows nothing on
	// it, so the agent is told "denied", not "no such server".
	Allowlist *mcpconf.Allowlist
}

// MCPListener is the agent's MCP listener, 127.0.0.1:8081 in its
// namespace, serving each of routes at MCPPath. Its filters, in order:
//
//   - header_mutation removes authorization and every header strip names,
//     from every request: the upstreams' credential is their own, and no
//     header the agent sends may change whose it is, or what a server
//     offers;
//   - the MCP filter parses the JSON-RPC body (REJECT_NO_MCP, BODY,
//     duplicate keys refused) and extracts method, params.name and the
//     arguments the allowlists constrain;
//   - RBAC allows, per upstream path (the whole :path, so no query
//     string), the lifecycle methods, GET and DELETE of the session without
//     a body, and tools/call of the agent's tools under their constraints;
//     nothing else;
//   - the router sends each path to its upstream's cluster, a route_for
//     tool to its own upstream's (targets, the upstreams that are up; a
//     target that is not gets a 503).
func MCPListener(netns, repo string, routes []MCPRoute, targets map[string]*mcpconf.Upstream, strip Strip) *listenerv3.Listener {
	routes = slices.Clone(routes)
	slices.SortFunc(routes, func(a, b MCPRoute) int { return strings.Compare(a.Upstream.Name, b.Upstream.Name) })
	name := ListenerName(repo, "mcp")
	filters := []*hcmv3.HttpFilter{
		httpFilter("envoy.filters.http.header_mutation", strip.mutation()),
		httpFilter(mcpFilter, mcpConfig(routes)),
		httpFilter("envoy.filters.http.rbac", mcpRBAC(routes)),
		httpFilter("envoy.filters.http.router", &routerv3.Router{}),
	}
	cfg := &hcmv3.HttpConnectionManager{
		StatPrefix:                   name,
		AccessLog:                    mcpAccessLog(repo),
		RouteSpecifier:               &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: mcpRoutes(routes, targets)},
		HttpFilters:                  filters,
		NormalizePath:                wrapperspb.Bool(true),
		MergeSlashes:                 true,
		PathWithEscapedSlashesAction: hcmv3.HttpConnectionManager_REJECT_REQUEST,
		StreamIdleTimeout:            durationpb.New(10 * time.Minute),
	}
	return &listenerv3.Listener{
		Name:         name,
		Address:      address(netns, MCPPort),
		FilterChains: []*listenerv3.FilterChain{{Filters: []*listenerv3.Filter{hcmFilter(cfg)}}},
	}
}

// Strip is what an MCP listener removes from every request before any
// other filter sees it: header families by prefix, any case, and exact
// names. authorization is always among the names.
type Strip struct {
	Prefixes []string
	Names    []string
}

// StripOf is the union of the upstreams' strip lines: one listener serves
// every upstream of an agent, and a header one upstream reads as
// configuration is no business of the agent's at any other.
func StripOf(upstreams []*mcpconf.Upstream) Strip {
	s := Strip{Names: []string{"authorization"}}
	for _, u := range upstreams {
		s.Prefixes = append(s.Prefixes, u.StripHeaderPrefixes...)
		s.Names = append(s.Names, u.StripHeaders...)
	}
	slices.Sort(s.Prefixes)
	slices.Sort(s.Names)
	s.Prefixes, s.Names = slices.Compact(s.Prefixes), slices.Compact(s.Names)
	return s
}

func (s Strip) mutation() *headermutationv3.HeaderMutation {
	var ms []*mutationv3.HeaderMutation
	for _, p := range s.Prefixes {
		ms = append(ms, &mutationv3.HeaderMutation{Action: &mutationv3.HeaderMutation_RemoveOnMatch_{
			RemoveOnMatch: &mutationv3.HeaderMutation_RemoveOnMatch{KeyMatcher: &matcherv3.StringMatcher{
				MatchPattern: &matcherv3.StringMatcher_Prefix{Prefix: p},
				IgnoreCase:   true,
			}},
		}})
	}
	names := s.Names
	if !slices.Contains(names, "authorization") {
		names = append([]string{"authorization"}, names...)
	}
	for _, n := range names {
		ms = append(ms, &mutationv3.HeaderMutation{Action: &mutationv3.HeaderMutation_Remove{Remove: n}})
	}
	return &headermutationv3.HeaderMutation{Mutations: &headermutationv3.Mutations{RequestMutations: ms}}
}

// mcpConfig is the MCP filter's configuration. Its body limit is the
// largest of the agent's upstreams': a per-route override would replace
// the whole configuration, modes included, and is not used.
func mcpConfig(routes []MCPRoute) *mcpv3.Mcp {
	limit := uint32(mcpconf.DefaultMaxRequestBytes)
	rules := []*mcpv3.ParserConfig_AttributeExtractionRule{{Path: "params.name"}}
	var args []string
	for i, r := range routes {
		if i == 0 || r.Upstream.MaxRequestBytes > limit {
			limit = r.Upstream.MaxRequestBytes
		}
		if r.Allowlist != nil {
			args = append(args, r.Allowlist.Arguments()...)
		}
	}
	slices.Sort(args)
	for _, a := range slices.Compact(args) {
		rules = append(rules, &mcpv3.ParserConfig_AttributeExtractionRule{Path: "params." + a})
	}
	return &mcpv3.Mcp{
		TrafficMode:         mcpv3.Mcp_REJECT_NO_MCP,
		AttributeSource:     mcpv3.Mcp_BODY,
		RejectDuplicateKeys: wrapperspb.Bool(true),
		ClearRouteCache:     true,
		MaxRequestBodySize:  wrapperspb.UInt32(limit),
		ParserConfig: &mcpv3.ParserConfig{Methods: []*mcpv3.ParserConfig_MethodConfig{{
			Method:          "tools/call",
			ExtractionRules: rules,
		}}},
	}
}

// mcpRBAC is the agent's allowlist as one ALLOW policy: per upstream path,
// what the agent may send there. With no permission at all, the policy is
// absent, and RBAC set but empty refuses everything.
func mcpRBAC(routes []MCPRoute) *rbacv3.RBAC {
	var perms []*rbacconfigv3.Permission
	for _, r := range routes {
		if r.Allowlist == nil {
			continue
		}
		allowed := []*rbacconfigv3.Permission{
			andPerm(orPerm(methodPerm("GET"), methodPerm("DELETE")), noBodyPerm()),
		}
		for _, m := range lifecycle {
			allowed = append(allowed, metadataPerm(m, "method"))
		}
		for _, set := range r.Allowlist.Sets {
			for _, t := range set.Tools {
				if len(t.Constraints) == 0 && !t.Unconstrained {
					// Empty constraints never mean any arguments: only
					// tool_unconstrained says that.
					continue
				}
				allowed = append(allowed, toolPerm(t))
			}
		}
		perms = append(perms, andPerm(pathPerm(MCPPath(r.Upstream.Name)), orPerm(allowed...)))
	}
	rules := &rbacconfigv3.RBAC{Action: rbacconfigv3.RBAC_ALLOW, Policies: map[string]*rbacconfigv3.Policy{}}
	if len(perms) > 0 {
		rules.Policies["allowlist"] = &rbacconfigv3.Policy{
			Permissions: perms,
			Principals:  []*rbacconfigv3.Principal{{Identifier: &rbacconfigv3.Principal_Any{Any: true}}},
		}
	}
	return &rbacv3.RBAC{Rules: rules}
}

// toolPerm allows tools/call of t, with each of its constraints.
func toolPerm(t mcpconf.Tool) *rbacconfigv3.Permission {
	rules := []*rbacconfigv3.Permission{
		metadataPerm("tools/call", "method"),
		metadataPerm(t.Name, "params", "name"),
	}
	for _, c := range t.Constraints {
		path := append([]string{"params"}, c.Path...)
		if c.Prefix {
			rules = append(rules, metadataMatch(&matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Prefix{Prefix: c.Value}}, path...))
		} else {
			rules = append(rules, metadataPerm(c.Value, path...))
		}
	}
	return andPerm(rules...)
}

func andPerm(rules ...*rbacconfigv3.Permission) *rbacconfigv3.Permission {
	return &rbacconfigv3.Permission{Rule: &rbacconfigv3.Permission_AndRules{AndRules: &rbacconfigv3.Permission_Set{Rules: rules}}}
}

func orPerm(rules ...*rbacconfigv3.Permission) *rbacconfigv3.Permission {
	return &rbacconfigv3.Permission{Rule: &rbacconfigv3.Permission_OrRules{OrRules: &rbacconfigv3.Permission_Set{Rules: rules}}}
}

func exact(s string) *matcherv3.StringMatcher {
	return &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: s}}
}

// metadataPerm matches the MCP filter's metadata at path, exactly value.
func metadataPerm(value string, path ...string) *rbacconfigv3.Permission {
	return metadataMatch(exact(value), path...)
}

func metadataMatch(m *matcherv3.StringMatcher, path ...string) *rbacconfigv3.Permission {
	segments := make([]*matcherv3.MetadataMatcher_PathSegment, len(path))
	for i, k := range path {
		segments[i] = &matcherv3.MetadataMatcher_PathSegment{Segment: &matcherv3.MetadataMatcher_PathSegment_Key{Key: k}}
	}
	return &rbacconfigv3.Permission{Rule: &rbacconfigv3.Permission_SourcedMetadata{SourcedMetadata: &rbacconfigv3.SourcedMetadata{
		MetadataMatcher: &matcherv3.MetadataMatcher{
			Filter: mcpFilter,
			Path:   segments,
			Value:  &matcherv3.ValueMatcher{MatchPattern: &matcherv3.ValueMatcher_StringMatch{StringMatch: m}},
		},
	}}}
}

func methodPerm(method string) *rbacconfigv3.Permission {
	return headerPerm(&routev3.HeaderMatcher{
		Name:                 ":method",
		HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{StringMatch: exact(method)},
	})
}

// pathPerm matches the request's :path exactly. Unlike url_path, :path
// holds the query string, so a request with any query matches no
// permission: MCP needs none, and the route would pass one on to the
// server.
func pathPerm(path string) *rbacconfigv3.Permission {
	return headerPerm(&routev3.HeaderMatcher{
		Name:                 ":path",
		HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{StringMatch: exact(path)},
	})
}

// noBodyPerm matches a request that announces no body: content-length
// absent or 0, and no transfer-encoding. The MCP filter passes GET and
// DELETE before reading a body, so a body on them would reach the server
// unparsed. (HTTP/2 can send DATA with neither header; that a server
// ignores such a body is the upgrade checklist's.)
func noBodyPerm() *rbacconfigv3.Permission {
	return andPerm(
		orPerm(
			headerPerm(&routev3.HeaderMatcher{Name: "content-length", HeaderMatchSpecifier: &routev3.HeaderMatcher_PresentMatch{PresentMatch: false}}),
			headerPerm(&routev3.HeaderMatcher{Name: "content-length", HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{StringMatch: exact("0")}}),
		),
		headerPerm(&routev3.HeaderMatcher{Name: "transfer-encoding", HeaderMatchSpecifier: &routev3.HeaderMatcher_PresentMatch{PresentMatch: false}}),
	)
}

func headerPerm(h *routev3.HeaderMatcher) *rbacconfigv3.Permission {
	return &rbacconfigv3.Permission{Rule: &rbacconfigv3.Permission_Header{Header: h}}
}

// mcpRoutes routes each upstream's path to its cluster, a route_for tool to
// its target's first, and anything else to a 404.
func mcpRoutes(routes []MCPRoute, targets map[string]*mcpconf.Upstream) *routev3.RouteConfiguration {
	var list []*routev3.Route
	for _, r := range routes {
		u := r.Upstream
		tools := ""
		if r.Allowlist != nil {
			tools = strings.Join(r.Allowlist.Tools(), ",")
		}
		for _, rf := range u.RouteFor {
			match := &routev3.RouteMatch{
				PathSpecifier: &routev3.RouteMatch_Path{Path: MCPPath(u.Name)},
				DynamicMetadata: []*matcherv3.MetadataMatcher{
					metadataMatcher("tools/call", "method"),
					metadataMatcher(rf.Tool, "params", "name"),
				},
			}
			t, ok := targets[rf.Upstream]
			if !ok {
				list = append(list, &routev3.Route{Match: match, Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{
					Status: 503,
					Body:   &corev3.DataSource{Specifier: &corev3.DataSource_InlineString{InlineString: "upstream " + rf.Upstream + " is not available\n"}},
				}}})
				continue
			}
			list = append(list, upstreamRoute(match, t, rf.Tool))
		}
		list = append(list, upstreamRoute(&routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Path{Path: MCPPath(u.Name)}}, u, tools))
	}
	list = append(list, &routev3.Route{
		Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
		Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: 404}},
	})
	return &routev3.RouteConfiguration{VirtualHosts: []*routev3.VirtualHost{{
		Name:    "mcp",
		Domains: []string{"*"},
		Routes:  list,
	}}}
}

// upstreamRoute sends match to u's cluster: the path rewritten to the
// server's, Host to its own address, u's per-agent headers set with tools.
func upstreamRoute(match *routev3.RouteMatch, u *mcpconf.Upstream, tools string) *routev3.Route {
	var add []*corev3.HeaderValueOption
	for _, h := range u.AgentHeaders {
		add = append(add, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: h.Name, Value: strings.ReplaceAll(h.Value, "{tools}", tools)},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	return &routev3.Route{
		Match: match,
		Action: &routev3.Route_Route{Route: &routev3.RouteAction{
			ClusterSpecifier:     &routev3.RouteAction_Cluster{Cluster: cds.MCPClusterName(u.Name)},
			PrefixRewrite:        u.Path,
			HostRewriteSpecifier: &routev3.RouteAction_HostRewriteLiteral{HostRewriteLiteral: "127.0.0.1:" + strconv.FormatUint(uint64(u.Port), 10)},
			Timeout:              durationpb.New(0),
		}},
		RequestHeadersToAdd: add,
	}
}

func metadataMatcher(value string, path ...string) *matcherv3.MetadataMatcher {
	return metadataPerm(value, path...).GetSourcedMetadata().GetMetadataMatcher()
}

// mcpAccessLog is the audit record (docs/mcp-upstreams.md, "Audit"): on
// top of the common fields, the request id, the cluster the request
// reached (which instance, under route_for), the MCP method and tool, the
// MCP filter's whole metadata (every extracted argument), and the response
// code's details, which tell RBAC's 403 (rbac_access_denied_...) from the
// server's.
func mcpAccessLog(repo string) []*accesslogv3.AccessLog {
	return accessLogFields(map[string]string{
		"agent": repo, "listener": "mcp",
		"request_id":   "%REQ(X-REQUEST-ID)%",
		"cluster":      "%UPSTREAM_CLUSTER%",
		"code_details": "%RESPONSE_CODE_DETAILS%",
		"mcp_method":   "%DYNAMIC_METADATA(" + mcpFilter + ":method)%",
		"mcp_tool":     "%DYNAMIC_METADATA(" + mcpFilter + ":params:name)%",
		"mcp":          "%DYNAMIC_METADATA(" + mcpFilter + ")%",
	})
}

// UpstreamEgressName names an upstream's egress listener.
func UpstreamEgressName(upstream string) string {
	return "upstream_" + upstream + "_egress"
}

// UpstreamEgressListener is upstream's egress: a CONNECT proxy on
// 127.0.0.1:3128 in the upstream's namespace, to port 443 of its egress
// hosts alone, as an agent's egress listener is.
func UpstreamEgressListener(netns string, u *mcpconf.Upstream) *listenerv3.Listener {
	name := UpstreamEgressName(u.Name)
	return connectListener(name, netns, u.Egress, accessLogFields(map[string]string{"upstream": u.Name, "listener": "egress"}))
}
