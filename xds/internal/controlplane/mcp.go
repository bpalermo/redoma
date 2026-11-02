package controlplane

import (
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"

	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/cds"
	"github.com/bpalermo/redoma/xds/internal/lds"
	"github.com/bpalermo/redoma/xds/internal/mcpconf"
)

// Host MCP upstreams (docs/mcp-upstreams.md). An upstream is up when its
// container is registered (agent.Agent.Upstream) and its <etc>/mcp/<name>.conf
// is valid: it then has its egress listener in its namespace and its
// cluster, and each agent with an allowlist file for it a route in the
// agent's MCP listener. Everything is read again at every scan, like the
// egress allowlists; nothing is published for an upstream that is not up.

// mcpDir is the upstreams' configuration.
func (c *Controller) mcpDir() mcpconf.Dir {
	return mcpconf.Dir{Etc: c.cfg.EtcDir, Owner: c.cfg.MCPOwner}
}

// applyUpstream starts or stops an upstream's container.
func (c *Controller) applyUpstream(e agent.Event) {
	if old, ok := c.upstreams[e.Agent.ID]; ok {
		delete(c.upstreams, e.Agent.ID)
		c.removedIn[old.Netns] = &removal{version: c.version + 1, since: time.Now(), held: old.NetnsID}
		c.dirty = true
		c.log.Info("MCP upstream stopped", "upstream", old.ID, "netns", old.Netns)
	}
	if e.Bound != nil {
		// An upstream's registrar does not wait: never leave one hanging.
		e.Bound <- nil
	}
	if !e.Removed {
		c.upstreams[e.Agent.ID] = e.Agent
		delete(c.removedIn, e.Agent.Netns)
		c.dirty = true
		c.log.Info("MCP upstream running", "upstream", e.Agent.ID, "netns", e.Agent.Netns)
	}
	c.readMCP()
}

// readMCP re-reads every upstream's file, then each agent's allowlists. A
// file that cannot be read is logged when its error changes; it never
// widens anything: an invalid upstream file publishes nothing of the
// upstream, an invalid allowlist keeps the agent's route and allows nothing.
func (c *Controller) readMCP() {
	dir := c.mcpDir()
	names, err := dir.Upstreams()
	confs := map[string]*mcpconf.Upstream{}
	errs := map[string]string{}
	if err != nil {
		errs[""] = err.Error()
	}
	for _, name := range names {
		u, err := dir.ReadUpstream(name)
		switch {
		case err != nil:
			errs[name] = err.Error()
		case u != nil:
			confs[name] = u
		}
	}
	c.logMCPErrors("MCP upstream unusable; nothing of it is published", "upstream", c.mcpErrs, errs)
	c.mcpErrs = errs
	if !reflect.DeepEqual(confs, c.mcpConfs) {
		c.mcpConfs = confs
		c.dirty = true
	}
	for _, a := range c.agents {
		c.readAgentMCP(a)
	}
}

// readAgentMCP reads a's allowlist for each upstream that is up.
func (c *Controller) readAgentMCP(a *running) {
	dir := c.mcpDir()
	var routes []lds.MCPRoute
	errs := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(c.upTargets())) {
		al, err := dir.ReadAllowlist(name, a.agent.ID)
		if err != nil {
			errs[name] = err.Error()
			routes = append(routes, lds.MCPRoute{Upstream: c.mcpConfs[name]})
			continue
		}
		if al != nil {
			routes = append(routes, lds.MCPRoute{Upstream: c.mcpConfs[name], Allowlist: al})
		}
	}
	c.logMCPErrors("MCP allowlist invalid; the agent's calls to the upstream are refused", "upstream", a.mcpErrs, errs, "agent", a.agent.ID)
	a.mcpErrs = errs
	if !reflect.DeepEqual(routes, a.mcp) {
		a.mcp = routes
		c.dirty = true
	}
}

// logMCPErrors logs each error of now that was not in before.
func (c *Controller) logMCPErrors(msg, key string, before, now map[string]string, args ...any) {
	for _, name := range slices.Sorted(maps.Keys(now)) {
		if before[name] != now[name] {
			c.log.Error(msg, append([]any{key, name, "err", now[name]}, args...)...)
		}
	}
}

// upTargets are the upstreams that are up, by name.
func (c *Controller) upTargets() map[string]*mcpconf.Upstream {
	out := map[string]*mcpconf.Upstream{}
	for name := range c.upstreams {
		if u, ok := c.mcpConfs[name]; ok {
			out[name] = u
		}
	}
	return out
}

// mcpStrip is what every agent's MCP listener removes: the union of every
// valid upstream file's strip lines, up or not.
func (c *Controller) mcpStrip() lds.Strip {
	return lds.StripOf(slices.Collect(maps.Values(c.mcpConfs)))
}

// mcpListeners are the agent's MCP listener, if it has an upstream, and
// every upstream's egress listener.
func (c *Controller) mcpAgentListener(id string, a *running) types.Resource {
	if len(a.mcp) == 0 {
		return nil
	}
	return lds.MCPListener(a.agent.Netns, id, a.mcp, c.upTargets(), c.mcpStrip())
}

func (c *Controller) mcpUpstreamListeners() []types.Resource {
	var out []types.Resource
	up := c.upTargets()
	for _, name := range slices.Sorted(maps.Keys(up)) {
		out = append(out, lds.UpstreamEgressListener(c.upstreams[name].Netns, up[name]))
	}
	return out
}

func (c *Controller) mcpClusters() []types.Resource {
	var out []types.Resource
	up := c.upTargets()
	for _, name := range slices.Sorted(maps.Keys(up)) {
		out = append(out, cds.MCPCluster(name, c.upstreams[name].Netns, up[name].Port))
	}
	return out
}
