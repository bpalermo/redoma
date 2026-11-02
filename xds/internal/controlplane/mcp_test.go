package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbacv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"github.com/bpalermo/redoma/xds/internal/agent"
)

// upstreamSource is the hook Source's part for upstreams: Synced at once,
// then whatever the test sends.
type upstreamSource struct{ ch chan agent.Event }

func (s *upstreamSource) Run(ctx context.Context, emit func(agent.Event)) error {
	emit(agent.Event{Synced: true})
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-s.ch:
			emit(e)
		}
	}
}

type mcpEnv struct {
	*env
	up  *upstreamSource
	etc string
}

func newMCPEnv(t *testing.T) *mcpEnv {
	t.Helper()
	up := &upstreamSource{ch: make(chan agent.Event, 8)}
	e := newEnvSources(t, func(c *Config) { c.MCPOwner = uint32(os.Getuid()) }, up)
	return &mcpEnv{env: e, up: up, etc: e.cfg.EtcDir}
}

func (m *mcpEnv) file(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(m.etc, "mcp", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, p, content)
}

const grafanaConf = "image = example.org/grafana@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `
port = 8000
path = /mcp
egress = grafana.example.com
strip_header_prefix = x-grafana-
`

func upstreamAgent(name, netns string) agent.Agent {
	return agent.Agent{ID: name, Netns: netns, NetnsID: agent.NetnsID{Dev: 9, Ino: 1}, Upstream: true}
}

func (m *mcpEnv) register(name, netns string) {
	m.up.ch <- agent.Event{Agent: upstreamAgent(name, netns)}
}

func (m *mcpEnv) unregister(name string) {
	m.up.ch <- agent.Event{Agent: agent.Agent{ID: name, Upstream: true}, Removed: true}
}

func has(names []string, name string) bool { return slices.Contains(names, name) }

func mcpRBACOf(t *testing.T, v view, name string) *rbacv3.RBAC {
	t.Helper()
	l := v.snap.GetResources(resource.ListenerType)[name].(*listenerv3.Listener)
	h := &hcmv3.HttpConnectionManager{}
	if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(h); err != nil {
		t.Fatal(err)
	}
	for _, f := range h.GetHttpFilters() {
		if f.GetName() == "envoy.filters.http.rbac" {
			r := &rbacv3.RBAC{}
			if err := f.GetTypedConfig().UnmarshalTo(r); err != nil {
				t.Fatal(err)
			}
			return r
		}
	}
	t.Fatal("no RBAC filter")
	return nil
}

func TestMCPUpstreamLifecycle(t *testing.T) {
	m := newMCPEnv(t)
	m.file(t, "grafana.conf", grafanaConf)
	m.file(t, "grafana/beta.conf", "tool_unconstrained = search_dashboards\n")
	m.startAgent(t, "beta", "")
	m.startAgent(t, "alpha", "")

	// Its file alone publishes nothing: the upstream's container is not
	// registered.
	v := m.eventually(t, "the agents", func(v view) bool {
		return has(v.listeners, "agent_beta_model") && has(v.listeners, "agent_alpha_model")
	})
	if has(v.listeners, "agent_beta_mcp") || has(clusters(v), "mcp_grafana") {
		t.Fatalf("an upstream not registered is published: %q %q", v.listeners, clusters(v))
	}

	m.register("grafana", "/run/netns/redoma-mcp-x")
	v = m.eventually(t, "the upstream", func(v view) bool {
		return has(v.listeners, "agent_beta_mcp") && has(v.listeners, "upstream_grafana_egress") && has(clusters(v), "mcp_grafana")
	})
	if has(v.listeners, "agent_alpha_mcp") {
		t.Error("an agent without an allowlist file got an MCP listener")
	}
	c := v.snap.GetResources(resource.ClusterType)["mcp_grafana"].(*clusterv3.Cluster)
	if c.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath() != "/run/netns/redoma-mcp-x" {
		t.Errorf("cluster bound in %v", c.GetUpstreamBindConfig())
	}
	eg := v.snap.GetResources(resource.ListenerType)["upstream_grafana_egress"].(*listenerv3.Listener)
	if eg.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath() != "/run/netns/redoma-mcp-x" {
		t.Errorf("egress listener bound in %v", eg.GetAddress())
	}
	if len(mcpRBACOf(t, v, "agent_beta_mcp").GetRules().GetPolicies()) != 1 {
		t.Error("beta's allowlist is not its RBAC policy")
	}

	// An allowlist added later is read at the next scan.
	m.file(t, "grafana/alpha.conf", "tool_unconstrained = search_dashboards\n")
	m.eventually(t, "alpha's route", func(v view) bool { return has(v.listeners, "agent_alpha_mcp") })

	// An invalid allowlist keeps the route and allows nothing.
	m.file(t, "grafana/beta.conf", "tool = a b\n")
	m.eventually(t, "beta denied", func(v view) bool {
		return has(v.listeners, "agent_beta_mcp") && len(mcpRBACOf(t, v, "agent_beta_mcp").GetRules().GetPolicies()) == 0
	})

	// An invalid upstream file publishes nothing of the upstream.
	m.file(t, "grafana.conf", grafanaConf+"colour = blue\n")
	v = m.eventually(t, "the upstream withdrawn", func(v view) bool {
		return !has(v.listeners, "agent_alpha_mcp") && !has(v.listeners, "upstream_grafana_egress") && !has(clusters(v), "mcp_grafana")
	})
	if !has(v.listeners, "agent_alpha_model") {
		t.Error("an invalid upstream file took the agent's other listeners")
	}
	m.file(t, "grafana.conf", grafanaConf)
	m.eventually(t, "the upstream back", func(v view) bool { return has(v.listeners, "agent_alpha_mcp") && has(clusters(v), "mcp_grafana") })

	// Its container stopping withdraws it.
	m.unregister("grafana")
	m.eventually(t, "the upstream stopped", func(v view) bool {
		return !has(v.listeners, "agent_alpha_mcp") && !has(v.listeners, "upstream_grafana_egress") && !has(clusters(v), "mcp_grafana")
	})
}

// An upstream and an agent of one name are two things.
func TestMCPUpstreamNamedLikeAnAgent(t *testing.T) {
	m := newMCPEnv(t)
	m.file(t, "beta.conf", strings.Replace(grafanaConf, "port = 8000", "port = 8001", 1))
	m.file(t, "beta/beta.conf", "tool_unconstrained = t\n")
	m.startAgent(t, "beta", "")
	m.register("beta", "/run/netns/redoma-mcp-y")
	m.eventually(t, "both", func(v view) bool {
		return has(v.listeners, "agent_beta_model") && has(v.listeners, "agent_beta_mcp") && has(v.listeners, "upstream_beta_egress") && has(clusters(v), "mcp_beta")
	})
	m.unregister("beta")
	v := m.eventually(t, "the upstream gone", func(v view) bool { return !has(v.listeners, "upstream_beta_egress") })
	if !has(v.listeners, "agent_beta_model") {
		t.Error("the upstream's removal stopped the agent")
	}
}

// Without <etc>/mcp, nothing of MCP is published.
func TestMCPWithoutTheDirectory(t *testing.T) {
	m := newMCPEnv(t)
	m.startAgent(t, "beta", "")
	m.register("grafana", "/run/netns/redoma-mcp-x")
	v := m.eventually(t, "beta", func(v view) bool { return has(v.listeners, "agent_beta_model") })
	for _, l := range v.listeners {
		if strings.Contains(l, "mcp") || strings.HasPrefix(l, "upstream_") {
			t.Errorf("listener %s without <etc>/mcp", l)
		}
	}
}
