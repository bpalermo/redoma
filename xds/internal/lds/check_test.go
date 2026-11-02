package lds

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
)

// TestCheckListener: a unix socket of mode 0666 (its directory is the
// boundary), routing the upstream's path to its cluster without the
// per-agent headers, and allowing only the session's start and
// tools/list, POSTed to that path.
func TestCheckListener(t *testing.T) {
	l := CheckListener(docsUp, CheckSocket("/run/redoma/xds/mcp-check", "docs"))
	if err := l.ValidateAll(); err != nil {
		t.Fatal(err)
	}
	pipe := l.GetAddress().GetPipe()
	if l.GetName() != "upstream_docs_check" || pipe.GetPath() != "/run/redoma/xds/mcp-check/mcp-check-docs.sock" || pipe.GetMode() != 0o666 {
		t.Errorf("listener %s at %v", l.GetName(), l.GetAddress())
	}
	h, names, _, m, r := mcpParts(t, l)
	if want := []string{mcpFilter, "envoy.filters.http.rbac", "envoy.filters.http.router"}; len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Errorf("filters %q, want %q", names, want)
	}
	if !m.GetRejectDuplicateKeys().GetValue() || m.GetTrafficMode().String() != "REJECT_NO_MCP" || m.GetAttributeSource().String() != "BODY" {
		t.Errorf("MCP filter %v", m)
	}
	routes := h.GetRouteConfig().GetVirtualHosts()[0].GetRoutes()
	if len(routes) != 2 || routes[0].GetMatch().GetPath() != "/mcp/docs" || routes[0].GetRoute().GetCluster() != "mcp_docs" ||
		routes[0].GetRoute().GetPrefixRewrite() != "/" || len(routes[0].GetRequestHeadersToAdd()) != 0 || routes[1].GetDirectResponse().GetStatus() != 404 {
		t.Errorf("routes %v", routes)
	}
	if h.GetPathWithEscapedSlashesAction() != hcmv3.HttpConnectionManager_REJECT_REQUEST {
		t.Error("escaped slashes are not refused")
	}
	p := MCPPath("docs")
	for _, tc := range []struct {
		name string
		req  request
		want bool
	}{
		{"initialize", lifecycleCall(p, "initialize"), true},
		{"initialized", lifecycleCall(p, "notifications/initialized"), true},
		{"ping", lifecycleCall(p, "ping"), true},
		{"tools/list", lifecycleCall(p, "tools/list"), true},
		{"tools/call", call(p, "read_page", nil), false},
		{"resources/list", lifecycleCall(p, "resources/list"), false},
		{"GET", request{path: p, httpMethod: "GET"}, false},
		{"GET, as if it had been parsed", request{path: p, httpMethod: "GET", meta: map[string]any{"method": "tools/list"}}, false},
		{"another path", lifecycleCall("/mcp/grafana", "tools/list"), false},
		{"a query", lifecycleCall(p+"?x=y", "tools/list"), false},
	} {
		if got := allows(t, r, tc.req); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, ok := l.GetAddress().GetAddress().(*corev3.Address_Pipe); !ok {
		t.Error("not a unix socket")
	}
}
