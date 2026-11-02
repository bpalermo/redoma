package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	rbacv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/mcpconf"
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
tool_check = off
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
	for _, c := range clusters(v) {
		if strings.HasPrefix(c, "mcp_") {
			t.Errorf("cluster %s without <etc>/mcp", c)
		}
	}
}

// upstreamEnv is a replaceEnv with grafana's file, whose upstream u the
// test registers itself.
func upstreamEnv(t *testing.T, cfg Config) (*replaceEnv, agent.Agent) {
	t.Helper()
	cfg.MCPOwner = uint32(os.Getuid())
	r := newReplaceEnv(t, cfg)
	p := filepath.Join(r.ctl.cfg.EtcDir, "mcp", "grafana.conf")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, p, grafanaConf)
	u := agent.Agent{ID: "grafana", Netns: "/run/netns/redoma-mcp-u", NetnsID: agent.NetnsID{Dev: 5, Ino: 1}, Upstream: true}
	return r, u
}

func egressOf(v view) bool { return has(v.listeners, "upstream_grafana_egress") }

// TestReplacedUpstreamNamespaceIsBoundAgain: an upstream's namespace
// replaced at the same path waits, as an agent's does, until Envoy has
// reported dropping the old listener and RebindGrace has passed: its
// egress listener is byte for byte the old one's, and would be served
// from the old socket.
func TestReplacedUpstreamNamespaceIsBoundAgain(t *testing.T) {
	r, u := upstreamEnv(t, Config{RebindTimeout: time.Hour, RebindGrace: 150 * time.Millisecond})
	r.src.events <- agent.Event{Agent: u}
	v := r.eventually(t, "the upstream", egressOf)
	r.ctl.ListenersApplied(v.snap.GetVersion(resource.ListenerType))

	u.NetnsID.Ino++
	r.src.events <- agent.Event{Agent: u}
	removed := r.eventually(t, "the upstream removed", func(v view) bool { return !egressOf(v) })
	for deadline := time.Now().Add(50 * time.Millisecond); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if egressOf(r.view()) {
			t.Fatal("bound again before Envoy reported the removal applied")
		}
	}
	r.ctl.ListenersApplied(removed.snap.GetVersion(resource.ListenerType))
	back := r.eventually(t, "the upstream again", egressOf)
	if back.snap.GetVersion(resource.ListenerType) == removed.snap.GetVersion(resource.ListenerType) {
		t.Error("bound again without a new version")
	}
}

// TestUnreportedUpstreamRemovalIsPublishedAgain: a waiting upstream keeps
// its path's removal alive, published again while Envoy does not report
// it, as a waiting agent does.
func TestUnreportedUpstreamRemovalIsPublishedAgain(t *testing.T) {
	r, u := upstreamEnv(t, Config{RebindTimeout: 20 * time.Millisecond, RebindGrace: time.Millisecond})
	r.src.events <- agent.Event{Agent: u}
	v := r.eventually(t, "the upstream", egressOf)
	r.ctl.ListenersApplied(v.snap.GetVersion(resource.ListenerType))

	u.NetnsID.Ino++
	r.src.events <- agent.Event{Agent: u}
	removed := r.eventually(t, "the upstream removed", func(v view) bool { return !egressOf(v) })
	first := removed.snap.GetVersion(resource.ListenerType)
	again := r.eventually(t, "the removal published again", func(v view) bool {
		return v.snap.GetVersion(resource.ListenerType) != first
	})
	if egressOf(again) {
		t.Fatal("bound again without a report")
	}
	r.ctl.ListenersApplied(first)
	r.eventually(t, "the upstream again", egressOf)
}

// An upstream in an agent's namespace, or an agent in an upstream's, is
// refused: its listeners would serve the other's processes.
func TestUpstreamAndAgentNeverShareANamespace(t *testing.T) {
	r, u := upstreamEnv(t, Config{RebindTimeout: time.Hour, RebindGrace: time.Millisecond})
	r.start(t)
	u.NetnsID = r.x.NetnsID
	r.src.events <- agent.Event{Agent: u}
	for deadline := time.Now().Add(100 * time.Millisecond); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if egressOf(r.view()) {
			t.Fatal("an upstream in an agent's namespace was published")
		}
	}

	r2, u2 := upstreamEnv(t, Config{RebindTimeout: time.Hour, RebindGrace: time.Millisecond})
	r2.src.events <- agent.Event{Agent: u2}
	r2.eventually(t, "the upstream", egressOf)
	x := r2.x
	x.NetnsID = u2.NetnsID
	r2.src.events <- agent.Event{Agent: x}
	for deadline := time.Now().Add(100 * time.Millisecond); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if has(r2.view().listeners, "agent_x_model") {
			t.Fatal("an agent in an upstream's namespace was published")
		}
	}
}

// checkedConf is grafanaConf under a tool check of mode.
func checkedConf(mode string) string {
	return strings.Replace(grafanaConf, "tool_check = off", "tool_check = "+mode, 1)
}

// fakeTools answers the tool check with tools, per upstream, counting the
// questions; an upstream it has no tools for fails.
type fakeTools struct {
	mu    sync.Mutex
	tools map[string][]string
	asked map[string]int
	paths []string
}

func (f *fakeTools) ask(_ context.Context, socket, path string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(socket), "mcp-check-"), ".sock")
	f.asked[name]++
	f.paths = append(f.paths, socket+" "+path)
	tools, ok := f.tools[name]
	if !ok {
		return nil, errors.New("nobody answers")
	}
	return tools, nil
}

func (f *fakeTools) set(name string, tools []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools[name] = tools
}

func (f *fakeTools) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[name]
}

func newCheckEnv(t *testing.T) (*mcpEnv, *fakeTools) {
	t.Helper()
	f := &fakeTools{tools: map[string][]string{}, asked: map[string]int{}}
	up := &upstreamSource{ch: make(chan agent.Event, 8)}
	e := newEnvSources(t, func(c *Config) {
		c.MCPOwner = uint32(os.Getuid())
		c.MCPCheckDir = "/run/redoma/xds/mcp-check"
		c.AskTools = f.ask
		c.CheckRetry = time.Millisecond
		c.CheckTimeout = 5 * time.Millisecond
	}, up)
	return &mcpEnv{env: e, up: up, etc: e.cfg.EtcDir}, f
}

// TestToolCheckRefuse: under refuse, nothing of the upstream but its check
// listener and cluster is published until it answers, through its check
// socket at its path; it is asked again while it fails. Once it answers, a
// tool an agent's own file names that it does not list makes that agent's
// list invalid, and one the defaults name every agent's.
func TestToolCheckRefuse(t *testing.T) {
	m, f := newCheckEnv(t)
	m.file(t, "grafana.conf", checkedConf("refuse"))
	m.file(t, "grafana/beta.conf", "tool_unconstrained = search_dashboards\n")
	m.file(t, "grafana/alpha.conf", "tool_unconstrained = search_dashboards\ntool_unconstrained = gone_tool\n")
	m.startAgent(t, "beta", "")
	m.startAgent(t, "alpha", "")
	m.register("grafana", "/run/netns/redoma-mcp-x")

	v := m.eventually(t, "the check listener and cluster", func(v view) bool {
		return has(v.listeners, "upstream_grafana_check") && has(clusters(v), "mcp_grafana")
	})
	if has(v.listeners, "agent_beta_mcp") || has(v.listeners, "upstream_grafana_egress") {
		t.Fatalf("published before the check answered: %q", v.listeners)
	}
	m.eventually(t, "asked again while it fails", func(view) bool { return f.count("grafana") >= 2 })
	f.mu.Lock()
	first := f.paths[0]
	f.mu.Unlock()
	if got := first; got != "/run/redoma/xds/mcp-check/mcp-check-grafana.sock /mcp/grafana" {
		t.Errorf("asked %q", got)
	}

	f.set("grafana", []string{"list_datasources", "search_dashboards"})
	v = m.eventually(t, "the upstream", func(v view) bool {
		return has(v.listeners, "agent_beta_mcp") && has(v.listeners, "upstream_grafana_egress") && has(v.listeners, "agent_alpha_mcp")
	})
	if len(mcpRBACOf(t, v, "agent_beta_mcp").GetRules().GetPolicies()) != 1 {
		t.Error("beta's list, all listed, is not its policy")
	}
	if len(mcpRBACOf(t, v, "agent_alpha_mcp").GetRules().GetPolicies()) != 0 {
		t.Error("alpha's list names a tool the server does not list, and is not refused")
	}
	asked := f.count("grafana")

	// The defaults name an unlisted tool: every agent's list is refused.
	m.file(t, "grafana/_default.conf", "tool_unconstrained = gone_tool\n")
	m.eventually(t, "every list refused", func(v view) bool {
		return has(v.listeners, "agent_beta_mcp") && len(mcpRBACOf(t, v, "agent_beta_mcp").GetRules().GetPolicies()) == 0
	})
	// The answer is kept for the container's life: not asked again.
	time.Sleep(50 * time.Millisecond)
	if got := f.count("grafana"); got != asked {
		t.Errorf("asked %d times after its answer, want %d", got, asked)
	}

	// Another container (a new namespace, at a path of its own): asked
	// again.
	m.up.ch <- agent.Event{Agent: agent.Agent{ID: "grafana", Netns: "/run/netns/redoma-mcp-y", NetnsID: agent.NetnsID{Dev: 9, Ino: 2}, Upstream: true}}
	m.eventually(t, "the new container asked", func(view) bool { return f.count("grafana") > asked })
}

// TestToolCheckRouteFor: under refuse, a route_for tool must be listed by
// its target, which must have answered, or the upstream is held back.
func TestToolCheckRouteFor(t *testing.T) {
	m, f := newCheckEnv(t)
	m.file(t, "grafana.conf", checkedConf("refuse")+"route_for.write_thing = writer\n")
	m.file(t, "writer.conf", strings.Replace(checkedConf("refuse"), "port = 8000", "port = 8001", 1))
	m.file(t, "grafana/beta.conf", "tool_unconstrained = search_dashboards\ntool_unconstrained = write_thing\n")
	m.startAgent(t, "beta", "")
	f.set("grafana", []string{"search_dashboards"})
	m.register("grafana", "/run/netns/redoma-mcp-x")
	m.up.ch <- agent.Event{Agent: agent.Agent{ID: "writer", Netns: "/run/netns/redoma-mcp-w", NetnsID: agent.NetnsID{Dev: 9, Ino: 7}, Upstream: true}}
	m.eventually(t, "both checked", func(view) bool { return f.count("grafana") >= 1 && f.count("writer") >= 1 })
	v := m.view()
	if has(v.listeners, "upstream_grafana_egress") {
		t.Fatal("published while its route_for target has not answered")
	}
	f.set("writer", []string{"other_tool"})
	m.eventually(t, "writer up", func(v view) bool { return has(v.listeners, "upstream_writer_egress") })
	time.Sleep(30 * time.Millisecond)
	if has(m.view().listeners, "upstream_grafana_egress") {
		t.Fatal("published while its target does not list the routed tool")
	}

	// Its target now lists it (another container of the writer).
	f.set("writer", []string{"write_thing"})
	m.up.ch <- agent.Event{Agent: agent.Agent{ID: "writer", Netns: "/run/netns/redoma-mcp-w", NetnsID: agent.NetnsID{Dev: 9, Ino: 8}, Upstream: true}}
	v = m.eventually(t, "grafana up", func(v view) bool {
		return has(v.listeners, "upstream_grafana_egress") && has(v.listeners, "agent_beta_mcp")
	})
	if len(mcpRBACOf(t, v, "agent_beta_mcp").GetRules().GetPolicies()) != 1 {
		t.Error("beta's list, its routed tool listed by the target, is refused")
	}
}

// TestToolCheckWarnAndOff: under warn, everything is published before and
// after the answer, mismatches or not; under off, nothing is asked, and no
// check listener is published.
func TestToolCheckWarnAndOff(t *testing.T) {
	m, f := newCheckEnv(t)
	m.file(t, "grafana.conf", checkedConf("warn"))
	m.file(t, "grafana/beta.conf", "tool_unconstrained = gone_tool\n")
	m.startAgent(t, "beta", "")
	m.register("grafana", "/run/netns/redoma-mcp-x")
	v := m.eventually(t, "published under warn", func(v view) bool {
		return has(v.listeners, "agent_beta_mcp") && has(v.listeners, "upstream_grafana_egress") && has(v.listeners, "upstream_grafana_check")
	})
	f.set("grafana", []string{"search_dashboards"})
	m.eventually(t, "asked", func(view) bool { return f.count("grafana") >= 1 })
	time.Sleep(30 * time.Millisecond)
	v = m.view()
	if !has(v.listeners, "agent_beta_mcp") || len(mcpRBACOf(t, v, "agent_beta_mcp").GetRules().GetPolicies()) != 1 {
		t.Error("a mismatch under warn refused beta's list")
	}

	m2, f2 := newCheckEnv(t)
	m2.file(t, "grafana.conf", checkedConf("off"))
	m2.file(t, "grafana/beta.conf", "tool_unconstrained = gone_tool\n")
	m2.startAgent(t, "beta", "")
	m2.register("grafana", "/run/netns/redoma-mcp-x")
	v = m2.eventually(t, "published under off", func(v view) bool { return has(v.listeners, "agent_beta_mcp") })
	time.Sleep(30 * time.Millisecond)
	if f2.count("grafana") != 0 || has(m2.view().listeners, "upstream_grafana_check") {
		t.Errorf("off: asked %d times, listeners %q", f2.count("grafana"), v.listeners)
	}
}

// allowlistMismatch says which file names the tools the upstream does not
// list: the defaults', when they name one, else the agent's own; under
// warn it is a warning, under off nothing.
func TestAllowlistMismatch(t *testing.T) {
	etc := t.TempDir()
	if err := os.Chmod(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Controller{
		cfg:      Config{EtcDir: etc, MCPOwner: uint32(os.Getuid())},
		mcpConfs: map[string]*mcpconf.Upstream{"g": {Name: "g", ToolCheck: mcpconf.ToolCheckRefuse}},
		checks:   map[string]*toolCheck{"g": {done: true, tools: []string{"a"}}},
	}
	if err := os.MkdirAll(filepath.Join(etc, "mcp", "g"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(etc, "mcp", "g", "_default.conf"), "tool_unconstrained = d\n")
	if err := os.Chmod(filepath.Join(etc, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(etc, "mcp", "g"), 0o755); err != nil {
		t.Fatal(err)
	}
	al := &mcpconf.Allowlist{Sets: []mcpconf.Set{{Tools: []mcpconf.Tool{{Name: "a", Unconstrained: true}, {Name: "d", Unconstrained: true}}}}}
	if refusal, _ := c.allowlistMismatch("g", al); !strings.HasPrefix(refusal, "_default.conf names tools the upstream does not list: d") {
		t.Errorf("the defaults' tool: %q", refusal)
	}
	own := &mcpconf.Allowlist{Sets: []mcpconf.Set{{Tools: []mcpconf.Tool{{Name: "a", Unconstrained: true}, {Name: "o", Unconstrained: true}}}}}
	if refusal, _ := c.allowlistMismatch("g", own); !strings.HasPrefix(refusal, "the agent's file names tools the upstream does not list: o") {
		t.Errorf("the agent's tool: %q", refusal)
	}
	c.mcpConfs["g"].ToolCheck = mcpconf.ToolCheckWarn
	if refusal, warning := c.allowlistMismatch("g", own); refusal != "" || !strings.Contains(warning, "o") {
		t.Errorf("warn: %q, %q", refusal, warning)
	}
	c.mcpConfs["g"].ToolCheck = mcpconf.ToolCheckOff
	if refusal, warning := c.allowlistMismatch("g", own); refusal != "" || warning != "" {
		t.Errorf("off: %q, %q", refusal, warning)
	}
}
