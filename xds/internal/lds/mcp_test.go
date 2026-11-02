package lds

import (
	"slices"
	"strings"
	"testing"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbacconfigv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	headermutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	mcpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/mcp/v3"
	rbacv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/bpalermo/redoma/xds/internal/mcpconf"
)

// The upstreams of the tests: grafana, with sets and a prefix constraint;
// docs, routing one tool to docs-writer.
var (
	grafanaUp = &mcpconf.Upstream{
		Name: "grafana", Port: 8000, Path: "/mcp", MaxRequestBytes: 1 << 20,
		StripHeaderPrefixes: []string{"x-grafana-"}, StripHeaders: []string{"authorization"},
	}
	docsUp = &mcpconf.Upstream{
		Name: "docs", Port: 8082, Path: "/", MaxRequestBytes: 2 << 20,
		StripHeaderPrefixes: []string{"x-mcp-"}, StripHeaders: []string{"authorization", "x-forwarded-user"},
		AgentHeaders: []mcpconf.Header{{Name: "x-mcp-tools", Value: "{tools}"}},
		RouteFor:     []mcpconf.Route{{Tool: "publish", Upstream: "docs-writer"}, {Tool: "delete_page", Upstream: "docs-admin"}},
	}
	docsWriterUp = &mcpconf.Upstream{Name: "docs-writer", Port: 8083, Path: "/rpc", MaxRequestBytes: 1 << 20}
)

func constraint(path, value string, prefix bool) mcpconf.Constraint {
	return mcpconf.Constraint{Path: strings.Split(path, "."), Value: value, Prefix: prefix}
}

func testRoutes() []MCPRoute {
	return []MCPRoute{
		{Upstream: grafanaUp, Allowlist: &mcpconf.Allowlist{Sets: []mcpconf.Set{
			{Tools: []mcpconf.Tool{
				{Name: "query_prometheus", Constraints: []mcpconf.Constraint{constraint("arguments.datasourceUid", "prom-main", false)}},
			}},
			{Tools: []mcpconf.Tool{
				{Name: "query_loki_logs", Constraints: []mcpconf.Constraint{
					constraint("arguments.datasourceUid", "loki-main", false),
					constraint("arguments.logql", "{app=", true),
				}},
				{Name: "search_dashboards"},
			}},
		}}},
		{Upstream: docsUp, Allowlist: &mcpconf.Allowlist{Sets: []mcpconf.Set{{Tools: []mcpconf.Tool{
			{Name: "read_page"}, {Name: "publish", Constraints: []mcpconf.Constraint{constraint("arguments.space", "agents/", true)}},
		}}}}},
	}
}

func testMCPListener() *listenerv3.Listener {
	return MCPListener("/run/netns/redoma-a", "a", testRoutes(),
		map[string]*mcpconf.Upstream{"docs-writer": docsWriterUp},
		StripOf([]*mcpconf.Upstream{grafanaUp, docsUp, docsWriterUp}))
}

// mcpParts unpacks the listener's HCM and its filters' configs.
func mcpParts(t *testing.T, l *listenerv3.Listener) (*hcmv3.HttpConnectionManager, []string, *headermutationv3.HeaderMutation, *mcpv3.Mcp, *rbacv3.RBAC) {
	t.Helper()
	h := &hcmv3.HttpConnectionManager{}
	if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(h); err != nil {
		t.Fatal(err)
	}
	var names []string
	hm, m, r := &headermutationv3.HeaderMutation{}, &mcpv3.Mcp{}, &rbacv3.RBAC{}
	for _, f := range h.GetHttpFilters() {
		names = append(names, f.GetName())
		switch f.GetName() {
		case "envoy.filters.http.header_mutation":
			if err := f.GetTypedConfig().UnmarshalTo(hm); err != nil {
				t.Fatal(err)
			}
		case mcpFilter:
			if err := f.GetTypedConfig().UnmarshalTo(m); err != nil {
				t.Fatal(err)
			}
		case "envoy.filters.http.rbac":
			if err := f.GetTypedConfig().UnmarshalTo(r); err != nil {
				t.Fatal(err)
			}
		}
	}
	return h, names, hm, m, r
}

// TestMCPListenerMatchesGolden: testdata/mcp.json is the listener of
// testMCPListener, field for field, read in review.
func TestMCPListenerMatchesGolden(t *testing.T) {
	got := testMCPListener()
	want := golden(t, "testdata/mcp.json")
	if diff := cmp.Diff(want, []*listenerv3.Listener{got}, protocmp.Transform()); diff != "" {
		t.Errorf("MCP listener differs from testdata/mcp.json (-golden +lds):\n%s", diff)
	}
}

func TestMCPListenerValidates(t *testing.T) {
	if err := testMCPListener().ValidateAll(); err != nil {
		t.Error(err)
	}
	if err := UpstreamEgressListener("/run/netns/redoma-mcp-x", grafanaUp).ValidateAll(); err != nil {
		t.Error(err)
	}
}

func TestMCPListenerFilterOrderAndModes(t *testing.T) {
	l := testMCPListener()
	if l.GetName() != "agent_a_mcp" || l.GetAddress().GetSocketAddress().GetPortValue() != MCPPort ||
		l.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath() != "/run/netns/redoma-a" {
		t.Errorf("listener %s at %v", l.GetName(), l.GetAddress())
	}
	h, names, _, m, _ := mcpParts(t, l)
	if want := []string{"envoy.filters.http.header_mutation", mcpFilter, "envoy.filters.http.rbac", "envoy.filters.http.router"}; !slices.Equal(names, want) {
		t.Errorf("filters %q, want %q", names, want)
	}
	if m.GetTrafficMode() != mcpv3.Mcp_REJECT_NO_MCP || m.GetAttributeSource() != mcpv3.Mcp_BODY ||
		!m.GetRejectDuplicateKeys().GetValue() || !m.GetClearRouteCache() {
		t.Errorf("MCP filter modes: %v", m)
	}
	if m.GetMaxRequestBodySize().GetValue() != 2<<20 {
		t.Errorf("body limit %d, want the largest upstream's", m.GetMaxRequestBodySize().GetValue())
	}
	var paths []string
	for _, r := range m.GetParserConfig().GetMethods()[0].GetExtractionRules() {
		paths = append(paths, r.GetPath())
	}
	if want := []string{"params.name", "params.arguments.datasourceUid", "params.arguments.logql", "params.arguments.space"}; !slices.Equal(paths, want) {
		t.Errorf("extracted %q, want %q", paths, want)
	}
	if !h.GetNormalizePath().GetValue() || !h.GetMergeSlashes() || h.GetPathWithEscapedSlashesAction() != hcmv3.HttpConnectionManager_REJECT_REQUEST {
		t.Errorf("path normalization: %v %v %v", h.GetNormalizePath(), h.GetMergeSlashes(), h.GetPathWithEscapedSlashesAction())
	}
}

// TestMCPListenerStripsHeaders: authorization and every upstream's
// families and names, whichever upstream the agent has.
func TestMCPListenerStripsHeaders(t *testing.T) {
	for _, routes := range [][]MCPRoute{testRoutes(), testRoutes()[:1], nil} {
		_, _, hm, _, _ := mcpParts(t, MCPListener("/n", "a", routes, nil, StripOf([]*mcpconf.Upstream{grafanaUp, docsUp})))
		var prefixes, names []string
		for _, m := range hm.GetMutations().GetRequestMutations() {
			if r := m.GetRemoveOnMatch(); r != nil {
				if !r.GetKeyMatcher().GetIgnoreCase() {
					t.Errorf("prefix %q is case sensitive", r.GetKeyMatcher().GetPrefix())
				}
				prefixes = append(prefixes, r.GetKeyMatcher().GetPrefix())
			} else {
				names = append(names, m.GetRemove())
			}
		}
		if !slices.Equal(prefixes, []string{"x-grafana-", "x-mcp-"}) || !slices.Equal(names, []string{"authorization", "x-forwarded-user"}) {
			t.Errorf("removes prefixes %q and names %q", prefixes, names)
		}
	}
	// authorization even when no upstream names it.
	_, _, hm, _, _ := mcpParts(t, MCPListener("/n", "a", nil, nil, Strip{}))
	if ms := hm.GetMutations().GetRequestMutations(); len(ms) != 1 || ms[0].GetRemove() != "authorization" {
		t.Errorf("an empty Strip removes %v", ms)
	}
}

// request is what Envoy's RBAC sees of an MCP request: the path, the
// method and the MCP filter's metadata (nested, as the filter writes it).
type request struct {
	path, httpMethod string
	meta             map[string]any
}

func call(path, tool string, args map[string]any) request {
	return request{path: path, httpMethod: "POST", meta: map[string]any{
		"method": "tools/call", "params": map[string]any{"name": tool, "arguments": args},
	}}
}

func lifecycleCall(path, method string) request {
	return request{path: path, httpMethod: "POST", meta: map[string]any{"method": method}}
}

// allows evaluates RBAC the way Envoy's does for this policy's matchers
// (an ALLOW action: allowed when any permission of any policy matches).
func allows(t *testing.T, r *rbacv3.RBAC, req request) bool {
	t.Helper()
	if r.GetRules().GetAction() != rbacconfigv3.RBAC_ALLOW {
		t.Fatalf("action %v", r.GetRules().GetAction())
	}
	for _, p := range r.GetRules().GetPolicies() {
		if len(p.GetPrincipals()) != 1 || !p.GetPrincipals()[0].GetAny() {
			t.Fatalf("principals %v, want any", p.GetPrincipals())
		}
		for _, perm := range p.GetPermissions() {
			if permMatches(t, perm, req) {
				return true
			}
		}
	}
	return false
}

func permMatches(t *testing.T, p *rbacconfigv3.Permission, req request) bool {
	t.Helper()
	switch {
	case p.GetAndRules() != nil:
		for _, r := range p.GetAndRules().GetRules() {
			if !permMatches(t, r, req) {
				return false
			}
		}
		return true
	case p.GetOrRules() != nil:
		for _, r := range p.GetOrRules().GetRules() {
			if permMatches(t, r, req) {
				return true
			}
		}
		return false
	case p.GetUrlPath() != nil:
		return stringMatches(t, p.GetUrlPath().GetPath(), req.path)
	case p.GetHeader() != nil:
		if p.GetHeader().GetName() != ":method" {
			t.Fatalf("header %q", p.GetHeader().GetName())
		}
		return stringMatches(t, p.GetHeader().GetStringMatch(), req.httpMethod)
	case p.GetSourcedMetadata() != nil:
		m := p.GetSourcedMetadata().GetMetadataMatcher()
		if m.GetFilter() != mcpFilter || p.GetSourcedMetadata().GetMetadataSource() != rbacconfigv3.MetadataSource_DYNAMIC {
			t.Fatalf("metadata of %q from %v", m.GetFilter(), p.GetSourcedMetadata().GetMetadataSource())
		}
		var v any = req.meta
		for _, seg := range m.GetPath() {
			obj, ok := v.(map[string]any)
			if !ok {
				return false
			}
			if v, ok = obj[seg.GetKey()]; !ok {
				return false
			}
		}
		s, ok := v.(string)
		return ok && stringMatches(t, m.GetValue().GetStringMatch(), s)
	}
	t.Fatalf("permission %v not modelled", p)
	return false
}

func stringMatches(t *testing.T, m interface {
	GetExact() string
	GetPrefix() string
	GetIgnoreCase() bool
}, s string,
) bool {
	t.Helper()
	if m.GetIgnoreCase() {
		t.Fatalf("matcher %v ignores case", m)
	}
	if e := m.GetExact(); e != "" {
		return s == e
	}
	if p := m.GetPrefix(); p != "" {
		return strings.HasPrefix(s, p)
	}
	t.Fatalf("matcher %v not modelled", m)
	return false
}

func TestMCPListenerRBAC(t *testing.T) {
	_, _, _, _, r := mcpParts(t, testMCPListener())
	g, d := MCPPath("grafana"), MCPPath("docs")
	prom := map[string]any{"datasourceUid": "prom-main"}
	for _, tc := range []struct {
		name string
		req  request
		want bool
	}{
		{"constrained tool, right datasource", call(g, "query_prometheus", prom), true},
		{"constrained tool, another datasource", call(g, "query_prometheus", map[string]any{"datasourceUid": "loki-main"}), false},
		{"constrained tool, argument missing", call(g, "query_prometheus", map[string]any{}), false},
		{"constrained tool, no arguments", call(g, "query_prometheus", nil), false},
		{"constrained tool, a number where a string is", call(g, "query_prometheus", map[string]any{"datasourceUid": 7.0}), false},
		{"a set's constraint does not leak to another set", call(g, "query_prometheus", map[string]any{"datasourceUid": "loki-main", "logql": "{app=x}"}), false},
		{"prefix constraint", call(g, "query_loki_logs", map[string]any{"datasourceUid": "loki-main", "logql": "{app=\"x\"}"}), true},
		{"prefix constraint, wrong prefix", call(g, "query_loki_logs", map[string]any{"datasourceUid": "loki-main", "logql": "sum({app=x})"}), false},
		{"unconstrained tool", call(g, "search_dashboards", nil), true},
		{"unlisted tool", call(g, "update_dashboard", nil), false},
		{"tool name case", call(g, "Search_Dashboards", nil), false},
		{"another upstream's tool on this path", call(g, "read_page", nil), false},
		{"this upstream's tool on another path", call(d, "search_dashboards", nil), false},
		{"other upstream, its own tool", call(d, "read_page", nil), true},
		{"route_for tool, constrained", call(d, "publish", map[string]any{"space": "agents/a"}), true},
		{"route_for tool, outside the constraint", call(d, "publish", map[string]any{"space": "root"}), false},
		{"route_for tool the agent lacks", call(d, "delete_page", nil), false},
		{"initialize", lifecycleCall(g, "initialize"), true},
		{"tools/list", lifecycleCall(g, "tools/list"), true},
		{"notifications/initialized", lifecycleCall(d, "notifications/initialized"), true},
		{"resources/read", lifecycleCall(g, "resources/read"), false},
		{"prompts/get", lifecycleCall(g, "prompts/get"), false},
		{"sampling", lifecycleCall(g, "sampling/createMessage"), false},
		{"no method (a response)", request{path: g, httpMethod: "POST", meta: map[string]any{}}, false},
		{"GET of the session", request{path: g, httpMethod: "GET"}, true},
		{"DELETE of the session", request{path: d, httpMethod: "DELETE"}, true},
		{"PUT", request{path: g, httpMethod: "PUT"}, false},
		{"another path", lifecycleCall("/mcp/nobody", "initialize"), false},
		{"a sub-path", call(g+"/x", "search_dashboards", nil), false},
		{"the root", lifecycleCall("/", "initialize"), false},
	} {
		if got := allows(t, r, tc.req); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An invalid allowlist keeps the route and allows nothing on it; the
// agent's other upstreams are unaffected.
func TestMCPListenerInvalidAllowlistDeniesItsUpstream(t *testing.T) {
	routes := testRoutes()
	routes[0].Allowlist = nil
	_, _, _, _, r := mcpParts(t, MCPListener("/n", "a", routes, nil, Strip{}))
	if allows(t, r, lifecycleCall(MCPPath("grafana"), "initialize")) || allows(t, r, call(MCPPath("grafana"), "search_dashboards", nil)) {
		t.Error("an invalid allowlist allows a call")
	}
	if !allows(t, r, call(MCPPath("docs"), "read_page", nil)) {
		t.Error("another upstream lost its tools")
	}
	routes[1].Allowlist = nil
	_, _, _, _, r = mcpParts(t, MCPListener("/n", "a", routes, nil, Strip{}))
	if r.GetRules() == nil || len(r.GetRules().GetPolicies()) != 0 {
		t.Errorf("no valid allowlist: rules %v, want set and empty (deny all)", r.GetRules())
	}
	if hasRoute(t, MCPListener("/n", "a", routes, nil, Strip{}), MCPPath("grafana")) == nil {
		t.Error("the invalid allowlist's route is gone")
	}
}

func hasRoute(t *testing.T, l *listenerv3.Listener, path string) *routev3.Route {
	t.Helper()
	h, _, _, _, _ := mcpParts(t, l)
	for _, r := range h.GetRouteConfig().GetVirtualHosts()[0].GetRoutes() {
		if r.GetMatch().GetPath() == path && len(r.GetMatch().GetDynamicMetadata()) == 0 {
			return r
		}
	}
	return nil
}

func TestMCPListenerRoutes(t *testing.T) {
	h, _, _, _, _ := mcpParts(t, testMCPListener())
	routes := h.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()
	type summary struct {
		path, tool, cluster, rewrite, host string
		status                             uint32
		headers                            string
	}
	var got []summary
	for _, r := range routes {
		s := summary{path: r.GetMatch().GetPath() + r.GetMatch().GetPrefix(), status: r.GetDirectResponse().GetStatus()}
		for _, m := range r.GetMatch().GetDynamicMetadata() {
			if m.GetPath()[0].GetKey() == "params" {
				s.tool = m.GetValue().GetStringMatch().GetExact()
			}
		}
		s.cluster, s.rewrite, s.host = r.GetRoute().GetCluster(), r.GetRoute().GetPrefixRewrite(), r.GetRoute().GetHostRewriteLiteral()
		for _, h := range r.GetRequestHeadersToAdd() {
			s.headers += h.GetHeader().GetKey() + "=" + h.GetHeader().GetValue() + ";"
		}
		got = append(got, s)
	}
	want := []summary{
		{path: "/mcp/docs", tool: "publish", cluster: "mcp_docs-writer", rewrite: "/rpc", host: "127.0.0.1:8083"},
		{path: "/mcp/docs", tool: "delete_page", status: 503},
		{path: "/mcp/docs", cluster: "mcp_docs", rewrite: "/", host: "127.0.0.1:8082", headers: "x-mcp-tools=publish,read_page;"},
		{path: "/mcp/grafana", cluster: "mcp_grafana", rewrite: "/mcp", host: "127.0.0.1:8000"},
		{path: "/", status: 404},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(summary{})); diff != "" {
		t.Errorf("routes (-want +got):\n%s", diff)
	}
}

// TestMCPListenerIsDeterministic: the same input is the same bytes, so a
// snapshot sent again does not look changed.
func TestMCPListenerIsDeterministic(t *testing.T) {
	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(testMCPListener())
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		routes := testRoutes()
		slices.Reverse(routes)
		again, err := proto.MarshalOptions{Deterministic: true}.Marshal(MCPListener("/run/netns/redoma-a", "a", routes,
			map[string]*mcpconf.Upstream{"docs-writer": docsWriterUp},
			StripOf([]*mcpconf.Upstream{docsWriterUp, docsUp, grafanaUp})))
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatal("the MCP listener differs from one build to the next")
		}
	}
}

func TestUpstreamEgressListener(t *testing.T) {
	u := &mcpconf.Upstream{Name: "grafana", Egress: []string{"grafana.example.com"}}
	l := UpstreamEgressListener("/run/netns/redoma-mcp-x", u)
	if l.GetName() != "upstream_grafana_egress" || l.GetAddress().GetSocketAddress().GetPortValue() != EgressPort ||
		l.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath() != "/run/netns/redoma-mcp-x" {
		t.Errorf("listener %s at %v", l.GetName(), l.GetAddress())
	}
	rc := egressRoutesOf(t, l)
	if vh := envoyVirtualHost(t, rc, "grafana.example.com:443"); vh != "allowed" {
		t.Errorf("its host: %q", vh)
	}
	if vh := envoyVirtualHost(t, rc, "api.example.com:443"); vh != "denied" {
		t.Errorf("another host: %q", vh)
	}
	// Its routes are an agent's egress listener's, for the same hosts.
	agent := egressRoutesOf(t, Listeners("/n", "a", 0, u.Egress)[1])
	if !proto.Equal(rc, agent) {
		t.Error("the upstream's egress routes differ from an agent's for the same hosts")
	}
}

func egressRoutesOf(t *testing.T, l *listenerv3.Listener) *routev3.RouteConfiguration {
	t.Helper()
	h := &hcmv3.HttpConnectionManager{}
	if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(h); err != nil {
		t.Fatal(err)
	}
	return h.GetRouteConfig()
}

// The body limit is the largest upstream's, wherever it sorts.
func TestMCPListenerBodyLimitIsTheLargest(t *testing.T) {
	zeta := *grafanaUp
	zeta.Name, zeta.MaxRequestBytes = "zeta", 3<<20
	_, _, _, m, _ := mcpParts(t, MCPListener("/n", "a", []MCPRoute{{Upstream: grafanaUp}, {Upstream: &zeta}}, nil, Strip{}))
	if got := m.GetMaxRequestBodySize().GetValue(); got != 3<<20 {
		t.Errorf("body limit %d, want %d", got, 3<<20)
	}
}
