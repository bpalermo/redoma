package controlplane

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/identity"
	"github.com/bpalermo/redoma/xds/internal/identity/identitytest"
)

// ackLikeEnvoy reports every snapshot version applied, as a connected Envoy
// that accepts everything does, until ctx ends.
func ackLikeEnvoy(ctx context.Context, c cache.SnapshotCache, ctl *Controller) {
	for ctx.Err() == nil {
		if snap, err := c.GetSnapshot(NodeKey); err == nil {
			ctl.ListenersApplied(snap.GetVersion(resource.ListenerType))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// replaceEnv is a controller fed by a chanSource, with no Envoy: the test
// reports the versions applied itself.
type replaceEnv struct {
	*env
	src  *chanSource
	ctl  *Controller
	fake *identitytest.Server
	sel  agent.Selector
	x    agent.Agent
	// stop ends the controller and waits for it.
	stop func()
}

func newReplaceEnv(t *testing.T, cfg Config) *replaceEnv {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	fake, conn := identitytest.Serve(t)
	ca := identitytest.NewCA(t, td)
	sel := agent.Selector{Type: "redoma", Value: "agent:x"}
	fake.SetSVIDs(sel.Type+":"+sel.Value, ca.SVID(t, "/agent/x"))
	cfg.EtcDir, cfg.TrustDomain, cfg.ScanInterval = t.TempDir(), td, 5*time.Millisecond
	// chanSource sends Synced at once: the timeout must not race it.
	cfg.SyncTimeout = time.Hour
	r := &replaceEnv{
		env:  &env{cache: cache.NewSnapshotCache(true, NodeHash{}, nil)},
		src:  &chanSource{events: make(chan agent.Event), err: make(chan error)},
		fake: fake,
		sel:  sel,
		x: agent.Agent{
			ID: "x", Netns: "/run/netns/redoma-x",
			NetnsID: agent.NetnsID{Dev: 4, Ino: 100}, Selectors: []agent.Selector{sel},
		},
	}
	r.ctl = New(cfg, r.src, identity.New(conn, td, log), r.cache, log)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.ctl.Run(ctx) }()
	var once sync.Once
	r.stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(r.stop)
	return r
}

func (r *replaceEnv) bound(v view) bool {
	return slices.Contains(v.listeners, "agent_x_model") && slices.Contains(v.secrets, "agent/x")
}

// start binds x, as Envoy reports applied.
func (r *replaceEnv) start(t *testing.T) {
	t.Helper()
	r.src.events <- agent.Event{Agent: r.x}
	v := r.eventually(t, "x bound", r.bound)
	r.ctl.ListenersApplied(v.snap.GetVersion(resource.ListenerType))
}

// replace puts another namespace at the path of x and returns the snapshot
// that removed x.
func (r *replaceEnv) replace(t *testing.T) view {
	t.Helper()
	r.x.NetnsID.Ino++
	r.src.events <- agent.Event{Agent: r.x}
	removed := r.eventually(t, "x removed", func(v view) bool { return !slices.Contains(v.listeners, "agent_x_model") })
	if slices.Contains(removed.secrets, "agent/x") {
		t.Error("the secret outlived the listeners")
	}
	return removed
}

// stillRemoved fails if x is bound again within d.
func (r *replaceEnv) stillRemoved(t *testing.T, d time.Duration, why string) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if slices.Contains(r.view().listeners, "agent_x_model") {
			t.Fatalf("x bound again %s", why)
		}
	}
}

// TestReplacedNamespaceIsBoundAgain: another namespace at the same path (the
// agent restarted between two scans) removes the agent's listeners, keeps
// them removed until Envoy reports the removal applied and RebindGrace has
// passed, then binds them again under a new version and resubscribes the
// identity. Envoy would otherwise hand the new listeners the old namespace's
// socket: the address, the path, is the same.
func TestReplacedNamespaceIsBoundAgain(t *testing.T) {
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour, RebindGrace: 150 * time.Millisecond})
	r.start(t)
	waitStreams(t, r.fake, r.sel.Type+":"+r.sel.Value, 1)

	removed := r.replace(t)
	r.stillRemoved(t, 50*time.Millisecond, "before Envoy reported the removal applied")

	r.ctl.ListenersApplied(removed.snap.GetVersion(resource.ListenerType))
	r.stillRemoved(t, 50*time.Millisecond, "within RebindGrace of the report")
	back := r.eventually(t, "x bound again", r.bound)
	if back.snap.GetVersion(resource.ListenerType) == removed.snap.GetVersion(resource.ListenerType) {
		t.Error("bound again without a new version")
	}
	if got := r.fake.Opened(r.sel.Type + ":" + r.sel.Value); got != 2 {
		t.Errorf("%d SVID subscriptions opened, want 2 (resubscribed)", got)
	}
	waitStreams(t, r.fake, r.sel.Type+":"+r.sel.Value, 1)
}

// TestVersionsOfAnotherProcessAreNotApplied: after a control-plane restart
// Envoy reports the version it last applied from the old process, often a
// higher number than any of the new one's. It must not count as applied.
func TestVersionsOfAnotherProcessAreNotApplied(t *testing.T) {
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour})
	r.start(t)
	for _, v := range []string{"1000", "0123456789ab-1000", "-1000", r.ctl.nonce + "-x", ""} {
		r.ctl.ListenersApplied(v)
	}
	removed := r.replace(t)
	r.stillRemoved(t, 100*time.Millisecond, "on another process's version")

	r.ctl.ListenersApplied(removed.snap.GetVersion(resource.ListenerType))
	r.eventually(t, "x bound again", r.bound)
}

// TestUnreportedRemovalIsPublishedAgain: a removal Envoy does not report
// within RebindTimeout is published again as a new version, and the agent
// stays removed until one of them is reported: binding without the report
// is the case that reuses the old socket.
func TestUnreportedRemovalIsPublishedAgain(t *testing.T) {
	r := newReplaceEnv(t, Config{RebindTimeout: 20 * time.Millisecond})
	r.start(t)
	removed := r.replace(t)
	first := removed.snap.GetVersion(resource.ListenerType)

	again := r.eventually(t, "the removal published again", func(v view) bool {
		return v.snap.GetVersion(resource.ListenerType) != first
	})
	if slices.Contains(again.listeners, "agent_x_model") {
		t.Fatal("x bound again without a report")
	}
	r.stillRemoved(t, 60*time.Millisecond, "without any report")

	// A slow Envoy reporting the first removal, older than the ones since,
	// clears the path: every version from it on lacks the listeners.
	r.ctl.ListenersApplied(first)
	r.eventually(t, "x bound again", r.bound)
}
