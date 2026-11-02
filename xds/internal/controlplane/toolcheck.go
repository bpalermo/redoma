package controlplane

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"

	"github.com/bpalermo/redoma/common/version"
	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/lds"
	"github.com/bpalermo/redoma/xds/internal/mcpconf"
	"github.com/bpalermo/redoma/xds/internal/toolcheck"
)

// The tool check (docs/mcp-upstreams.md, "Who enables what"). When an
// upstream's container registers, and when redoma xds starts, it asks the
// upstream for its tools, through Envoy (lds.CheckListener), and compares
// them with every tool name its configuration names. The answer is kept for
// the container's life. The upstream's tool_check decides what a mismatch
// does:
//
//   - refuse: the upstream is not published until it has answered (it is
//     asked again at each scan until it does); a route_for tool its target
//     does not list fails the upstream; a tool the defaults name that it
//     does not list makes every agent's list for it invalid, one an agent's
//     own file names that agent's list alone;
//   - warn: the mismatches are logged, and everything is published as if
//     there were none;
//   - off: nothing is asked.
//
// Tools the server lists and no file names are logged, whatever the mode.

// toolCheck is the answer of one upstream's container.
type toolCheck struct {
	netns agent.NetnsID
	// asking while a question is out; done once answered, tools the
	// answer; err the last failure, failed when it came.
	asking bool
	done   bool
	tools  []string
	err    string
	failed time.Time
}

// askTools asks upstream for its tools through its check socket.
func (c *Controller) askTools(ctx context.Context, socket, path string) ([]string, error) {
	if c.cfg.AskTools != nil {
		return c.cfg.AskTools(ctx, socket, path)
	}
	return toolcheck.Ask(ctx, toolcheck.Unix(socket), path, version.Version)
}

// checking reports whether upstream name is registered with a valid file
// whose tool_check is not off: its check listener and cluster are
// published, up or not.
func (c *Controller) checking(name string) bool {
	_, ok := c.upstreams[name]
	conf := c.mcpConfs[name]
	return ok && conf != nil && conf.ToolCheck != mcpconf.ToolCheckOff
}

// updateChecks forgets the answers of containers that are gone, and asks
// each upstream being checked that has no answer and no question out (one
// that failed, once a scan has passed).
func (c *Controller) updateChecks() {
	for name, ch := range c.checks {
		if !c.checking(name) || c.upstreams[name].NetnsID != ch.netns {
			delete(c.checks, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.upstreams)) {
		if !c.checking(name) {
			continue
		}
		u := c.upstreams[name]
		ch := c.checks[name]
		if ch == nil {
			ch = &toolCheck{netns: u.NetnsID}
			c.checks[name] = ch
		}
		if ch.done || ch.asking || (!ch.failed.IsZero() && time.Since(ch.failed) < c.cfg.ScanInterval) || c.ctx == nil {
			continue
		}
		ch.asking = true
		go c.ask(c.ctx, name, u.NetnsID)
	}
}

// ask asks name's container (netns) for its tools, again every CheckRetry
// for CheckTimeout while it fails (Envoy may not have bound the check
// listener yet), and hands the answer to Run.
func (c *Controller) ask(ctx context.Context, name string, netns agent.NetnsID) {
	retry, timeout := c.cfg.CheckRetry, c.cfg.CheckTimeout
	if retry <= 0 {
		retry = time.Second
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	socket := lds.CheckSocket(c.cfg.MCPCheckDir, name)
	for {
		tools, err := c.askTools(ctx, socket, lds.MCPPath(name))
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			c.send(ctx, func(c *Controller) { c.checked(name, netns, tools, err) })
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// checked records an answer, if it is still the container's.
func (c *Controller) checked(name string, netns agent.NetnsID, tools []string, err error) {
	ch := c.checks[name]
	if ch == nil || ch.netns != netns {
		return
	}
	ch.asking = false
	if err != nil {
		ch.err, ch.failed = err.Error(), time.Now()
		c.log.Error("MCP upstream's tool check failed; asked again at the next scan", "upstream", name, "err", err)
	} else {
		ch.done, ch.tools, ch.err = true, tools, ""
		c.log.Info("MCP upstream's tools", "upstream", name, "tools", strings.Join(tools, ","))
		c.reportUnnamed(name)
	}
	c.readMCP()
}

// toolsOf is what upstream name lists: tools; all when its check is off
// (anything is taken as listed); known false while that is not known (its
// container not registered, or not answered). An upstream without a valid
// file lists nothing, known.
func (c *Controller) toolsOf(name string) (tools []string, all, known bool) {
	conf := c.mcpConfs[name]
	switch {
	case conf == nil:
		return nil, false, true
	case conf.ToolCheck == mcpconf.ToolCheckOff:
		return nil, true, true
	}
	ch := c.checks[name]
	if ch == nil || !ch.done {
		return nil, false, false
	}
	return ch.tools, false, true
}

// listed reports whether a tool named under upstream name is listed by the
// upstream that serves it: its route_for target's, or its own.
func (c *Controller) listed(name, tool string) (listed, known bool) {
	if conf := c.mcpConfs[name]; conf != nil {
		for _, rf := range conf.RouteFor {
			if rf.Tool == tool {
				name = rf.Upstream
				break
			}
		}
	}
	tools, all, known := c.toolsOf(name)
	return all || slices.Contains(tools, tool), known
}

// unlisted are the tools of names that upstream name's server does not
// list, and whether that is known.
func (c *Controller) unlisted(name string, names []string) ([]string, bool) {
	var out []string
	for _, t := range names {
		ok, known := c.listed(name, t)
		if !known {
			return nil, false
		}
		if !ok {
			out = append(out, t)
		}
	}
	return out, true
}

// checkBlocks are the upstreams under refuse that may not be published,
// with why: not answered, or a route_for tool its target does not list.
func (c *Controller) checkBlocks() map[string]string {
	out := map[string]string{}
	for name := range c.upstreams {
		conf := c.mcpConfs[name]
		if conf == nil || conf.ToolCheck != mcpconf.ToolCheckRefuse {
			continue
		}
		if ch := c.checks[name]; ch == nil || !ch.done {
			out[name] = "the tool check has not answered yet"
			if ch != nil && ch.err != "" {
				out[name] = "the tool check failed: " + ch.err
			}
			continue
		}
		var routed []string
		for _, rf := range conf.RouteFor {
			routed = append(routed, rf.Tool)
		}
		missing, known := c.unlisted(name, routed)
		switch {
		case !known:
			out[name] = "a route_for target's tool check has not answered yet"
		case len(missing) > 0:
			out[name] = fmt.Sprintf("route_for names tools their target does not list: %s", strings.Join(missing, ","))
		}
	}
	return out
}

// warnMismatches logs, for each upstream under warn that has answered, the
// tools its file's route_for lines and its defaults name that it does not
// list (an agent's own are readAgentMCP's), when that changes.
func (c *Controller) warnMismatches() {
	warns := map[string]string{}
	dir := c.mcpDir()
	for name := range c.upstreams {
		conf := c.mcpConfs[name]
		if conf == nil || conf.ToolCheck != mcpconf.ToolCheckWarn {
			continue
		}
		var names []string
		for _, rf := range conf.RouteFor {
			names = append(names, rf.Tool)
		}
		if d, err := dir.ReadDefaults(name); err == nil && d != nil {
			names = append(names, d.Tools()...)
		}
		if missing, known := c.unlisted(name, names); known && len(missing) > 0 {
			warns[name] = strings.Join(missing, ",")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(warns)) {
		if c.mcpWarns[name] != warns[name] {
			c.log.Warn("MCP upstream's files name tools it does not list (tool_check = warn: published anyway)", "upstream", name, "tools", warns[name])
		}
	}
	c.mcpWarns = warns
}

// allowlistMismatch checks an agent's allowlist for upstream name against
// what it lists: under refuse, a tool it does not list makes the list
// invalid (the error says whether the defaults or the agent's own file
// names it); under warn, it is only returned as a warning.
func (c *Controller) allowlistMismatch(name string, al *mcpconf.Allowlist) (refusal, warning string) {
	conf := c.mcpConfs[name]
	if conf == nil || conf.ToolCheck == mcpconf.ToolCheckOff || al == nil {
		return "", ""
	}
	missing, known := c.unlisted(name, al.Tools())
	if !known || len(missing) == 0 {
		return "", ""
	}
	which := "the agent's file"
	if d, err := c.mcpDir().ReadDefaults(name); err == nil && d != nil {
		for _, t := range missing {
			if slices.Contains(d.Tools(), t) {
				which = "_default.conf"
				break
			}
		}
	}
	msg := fmt.Sprintf("%s names tools the upstream does not list: %s", which, strings.Join(missing, ","))
	if conf.ToolCheck == mcpconf.ToolCheckRefuse {
		return msg, ""
	}
	return "", msg
}

// reportUnnamed logs the tools upstream name lists that no file names: its
// defaults, an agent's file, or another upstream's route_for.
func (c *Controller) reportUnnamed(name string) {
	ch := c.checks[name]
	named := c.namedTools(name)
	var unnamed []string
	for _, t := range ch.tools {
		if !named[t] {
			unnamed = append(unnamed, t)
		}
	}
	if len(unnamed) > 0 {
		c.log.Warn("MCP upstream lists tools no file names (registered without anyone choosing them)", "upstream", name, "tools", strings.Join(unnamed, ","))
	}
}

// namedTools are the tools the files name for upstream name: another
// upstream's route_for to it, its defaults, each agent's file (a readable
// one).
func (c *Controller) namedTools(name string) map[string]bool {
	dir := c.mcpDir()
	named := map[string]bool{}
	for _, conf := range c.mcpConfs {
		for _, rf := range conf.RouteFor {
			if rf.Upstream == name {
				named[rf.Tool] = true
			}
		}
	}
	lists := []*mcpconf.Allowlist{}
	if d, err := dir.ReadDefaults(name); err == nil {
		lists = append(lists, d)
	}
	agents, _ := dir.Agents(name)
	for _, a := range agents {
		if al, err := dir.ReadAllowlist(name, a); err == nil {
			lists = append(lists, al)
		}
	}
	for _, al := range lists {
		if al == nil {
			continue
		}
		for _, t := range al.Tools() {
			named[t] = true
		}
	}
	return named
}

// mcpCheckListeners are the check listeners of the upstreams being
// checked.
func (c *Controller) mcpCheckListeners() []types.Resource {
	var out []types.Resource
	for _, name := range slices.Sorted(maps.Keys(c.upstreams)) {
		if c.checking(name) {
			out = append(out, lds.CheckListener(c.mcpConfs[name], lds.CheckSocket(c.cfg.MCPCheckDir, name)))
		}
	}
	return out
}
