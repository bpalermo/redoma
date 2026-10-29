package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bpalermo/redoma/xds/internal/agent"
)

// Binding an agent again at a namespace path.
//
// Envoy hands a new listener the socket of a live or draining listener at a
// compatible address, and the address is the namespace PATH: a namespace
// replaced at the same path would be served from the old socket, in the old
// namespace. So the listeners at a path are removed first, and an agent is
// bound there again only once the path is clear:
//
//  1. the removal is published as version v;
//  2. Envoy reports v (or later) applied: the version_info of its next SotW
//     LDS request (server.ListenerVersions calls ListenersApplied);
//  3. RebindGrace passes. Envoy closes a removed listener's sockets only
//     after every worker has stopped listening on it, and that can come
//     after the report. The grace makes reuse of the old socket unlikely,
//     not impossible: a main dispatcher that has not closed the socket
//     after RebindGrace would still hand it over.
//
// A removal Envoy does not report within RebindTimeout is published again,
// as a new version, for as long as an agent waits on the path: binding
// without the report is exactly the case that reuses the old socket.

// removal is the removal of the listeners at one namespace path.
type removal struct {
	version uint64    // the first version without them
	since   time.Time // when it was (last) published
	acked   time.Time // when Envoy reported it applied; zero until then
	// held is the namespace whose listeners are being removed. Binding an
	// agent in that same namespace needs no wait: a socket Envoy hands over
	// is in the right namespace.
	held agent.NetnsID
}

// newNonce is a per-process prefix for versions.
func newNonce() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on Linux; a clock is unique enough.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// versionString is the version_info of snapshot n of this process.
func (c *Controller) versionString(n uint64) string {
	return c.nonce + "-" + strconv.FormatUint(n, 10)
}

// ListenersApplied records that Envoy applied the listeners of version (the
// version_info of its LDS requests). A version of another process, which
// Envoy reports after the control plane restarted, says nothing about this
// one's snapshots and is ignored. Safe from any goroutine; never blocks.
func (c *Controller) ListenersApplied(version string) {
	prefix, n, ok := strings.Cut(version, "-")
	if !ok || prefix != c.nonce {
		return
	}
	v, err := strconv.ParseUint(n, 10, 64)
	if err != nil {
		return
	}
	for {
		old := c.applied.Load()
		if v <= old || c.applied.CompareAndSwap(old, v) {
			break
		}
	}
	c.wake()
}

// wake has Run look at the waiting agents again. Never blocks.
func (c *Controller) wake() {
	select {
	case c.wakeC <- struct{}{}:
	default:
	}
}

func (c *Controller) apply(ctx context.Context, e agent.Event) {
	if e.Synced {
		c.synced, c.dirty = true, true
		return
	}
	if _, ok := c.bound[e.Agent.ID]; ok {
		c.failBound(e.Agent.ID, errors.New("the agent was stopped or replaced before Envoy bound its listeners"))
	}
	if e.Bound != nil && !e.Removed {
		c.bound[e.Agent.ID] = &boundWait{ch: e.Bound}
	}
	if old, ok := c.agents[e.Agent.ID]; ok {
		c.stop(old)
	}
	delete(c.waiting, e.Agent.ID)
	if e.Removed || c.refuseShared(e.Agent) {
		return
	}
	if !c.pathClear(e.Agent, time.Now()) {
		c.waiting[e.Agent.ID] = e.Agent
		c.log.Info("agent waits for Envoy to drop the listeners at its namespace path", "agent", e.Agent.ID, "netns", e.Agent.Netns)
		return
	}
	c.start(ctx, e.Agent)
}

// refuseShared refuses an agent whose namespace another agent already has
// (a container started with --network ns:<another agent's namespace>, or
// --network container:, or a pod): its listeners would serve the other
// agent's processes, and the duplicate addresses would make Envoy refuse the
// whole listener update. A refused start fails its Bound.
func (c *Controller) refuseShared(a agent.Agent) bool {
	if a.NetnsID == (agent.NetnsID{}) {
		return false
	}
	holder := ""
	for id, r := range c.agents {
		if id != a.ID && r.agent.NetnsID == a.NetnsID {
			holder = id
		}
	}
	for id, w := range c.waiting {
		if id != a.ID && w.NetnsID == a.NetnsID {
			holder = id
		}
	}
	if holder == "" {
		return false
	}
	err := fmt.Errorf("the network namespace at %s is agent %s's already", a.Netns, holder)
	c.log.Error("refusing an agent in another agent's network namespace", "agent", a.ID, "netns", a.Netns, "holder", holder)
	if _, ok := c.bound[a.ID]; ok {
		c.failBound(a.ID, err)
	}
	return true
}

// pathClear reports whether a's listeners can be bound at its path: no
// removal there, one of a's own namespace, or one Envoy reported applied at
// least RebindGrace ago.
func (c *Controller) pathClear(a agent.Agent, now time.Time) bool {
	r, ok := c.removedIn[a.Netns]
	if !ok || r.held == a.NetnsID {
		return true
	}
	return r.reported(now, c.cfg.RebindGrace)
}

func (r *removal) reported(now time.Time, grace time.Duration) bool {
	return !r.acked.IsZero() && now.Sub(r.acked) >= grace
}

// rebindWaiting moves the removals on (reported, overdue) and starts the
// waiting agents whose path is clear.
func (c *Controller) rebindWaiting(ctx context.Context) {
	now := time.Now()
	waitedOn := map[string]bool{}
	for _, a := range c.waiting {
		waitedOn[a.Netns] = true
	}
	for path, r := range c.removedIn {
		c.advance(path, r, now, waitedOn[path])
	}
	for id, a := range c.waiting {
		if c.pathClear(a, now) {
			delete(c.waiting, id)
			delete(c.removedIn, a.Netns)
			c.start(ctx, a)
		}
	}
	// A removal cleared and forgotten above publishes nothing by itself.
	c.save()
	c.confirmBound()
}

// boundWait is a start whose Source waits for Envoy to bind it.
type boundWait struct {
	ch      chan<- error
	version uint64 // the first version with its listeners; 0 until started
}

// confirmBound tells the Sources waiting on agents whose listeners Envoy
// has reported applied.
func (c *Controller) confirmBound() {
	applied := c.applied.Load()
	for id, b := range c.bound {
		if b.version != 0 && applied >= b.version {
			b.ch <- nil
			delete(c.bound, id)
		}
	}
}

func (c *Controller) failBound(id string, err error) {
	c.bound[id].ch <- err
	delete(c.bound, id)
}

// advance records Envoy's report of a removal, forgets a clear one nobody
// waits on, and publishes again one that is overdue and waited on.
func (c *Controller) advance(path string, r *removal, now time.Time, waitedOn bool) {
	switch {
	case r.acked.IsZero() && c.applied.Load() >= r.version:
		r.acked = now
		time.AfterFunc(c.cfg.RebindGrace, c.wake)
	case !r.acked.IsZero():
		if !waitedOn && r.reported(now, c.cfg.RebindGrace) {
			delete(c.removedIn, path)
		}
	case waitedOn && now.Sub(r.since) >= c.cfg.RebindTimeout:
		c.log.Warn("Envoy has not reported dropping the listeners at a namespace path; removing them again", "netns", path, "version", c.versionString(r.version))
		// The snapshot publish sets next has the same content and a new
		// version. r.version stays: every version from it on lacks the
		// listeners at path (nothing binds there while it is not clear), so
		// a report of any of them clears it, and a slow Envoy is never
		// chased by ever newer versions.
		r.since = now
		c.dirty = true
	}
}
