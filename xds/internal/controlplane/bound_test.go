package controlplane

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"github.com/bpalermo/redoma/xds/internal/agent"
)

func recv(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("no answer on Bound")
		return nil
	}
}

// TestBoundIsConfirmedOnEnvoysReport: a Source waiting on Bound hears nothing
// until Envoy reports a snapshot with the agent's listeners applied, then nil.
func TestBoundIsConfirmedOnEnvoysReport(t *testing.T) {
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour})
	bound := make(chan error, 1)
	r.src.events <- agent.Event{Agent: r.x, Bound: bound}
	v := r.eventually(t, "x bound", r.bound)

	select {
	case err := <-bound:
		t.Fatalf("Bound answered %v before Envoy reported anything", err)
	case <-time.After(50 * time.Millisecond):
	}
	// An earlier process's version says nothing.
	r.ctl.ListenersApplied("0123456789ab-1000")
	select {
	case err := <-bound:
		t.Fatalf("Bound answered %v on another process's version", err)
	case <-time.After(50 * time.Millisecond):
	}
	r.ctl.ListenersApplied(v.snap.GetVersion(resource.ListenerType))
	if err := recv(t, bound); err != nil {
		t.Errorf("Bound = %v, want nil", err)
	}
}

// TestBoundFailsWhenReplaced: an agent stopped before Envoy bound it fails
// its Bound, so the registrar does not report a start that never happened.
func TestBoundFailsWhenReplaced(t *testing.T) {
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour})
	bound := make(chan error, 1)
	r.src.events <- agent.Event{Agent: r.x, Bound: bound}
	r.eventually(t, "x bound", r.bound)
	r.src.events <- agent.Event{Agent: r.x, Removed: true}
	if err := recv(t, bound); err == nil {
		t.Error("Bound = nil for an agent removed before Envoy reported it")
	}
}

// TestBoundWaitsForARebind: an agent waiting to be bound again at a replaced
// path is confirmed only after its own listeners are reported, not on the
// report of the removal.
func TestBoundWaitsForARebind(t *testing.T) {
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour})
	r.start(t)
	r.x.NetnsID.Ino++
	bound := make(chan error, 1)
	r.src.events <- agent.Event{Agent: r.x, Bound: bound}
	removed := r.eventually(t, "x removed", func(v view) bool { return !r.bound(v) })

	r.ctl.ListenersApplied(removed.snap.GetVersion(resource.ListenerType))
	back := r.eventually(t, "x bound again", r.bound)
	select {
	case err := <-bound:
		t.Fatalf("Bound answered %v on the report of the removal", err)
	case <-time.After(50 * time.Millisecond):
	}
	r.ctl.ListenersApplied(back.snap.GetVersion(resource.ListenerType))
	if err := recv(t, bound); err != nil {
		t.Errorf("Bound = %v, want nil", err)
	}
}

// TestSharedNamespaceIsRefused: a container started in a launcher agent's
// namespace (--network ns:/run/netns/redoma-x) and registered as another
// agent is refused: its Bound fails and it gets no listeners, and x keeps
// its own.
func TestSharedNamespaceIsRefused(t *testing.T) {
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour})
	r.start(t)
	intruder := agent.Agent{
		ID:        "y",
		Netns:     "/run/netns/redoma-oci-0123",
		NetnsID:   r.x.NetnsID,
		Selectors: []agent.Selector{{Type: "redoma", Value: "agent:y"}},
	}
	bound := make(chan error, 1)
	r.src.events <- agent.Event{Agent: intruder, Bound: bound}
	if err := recv(t, bound); err == nil || !strings.Contains(err.Error(), "agent x's already") {
		t.Fatalf("Bound = %v, want the shared namespace refused", err)
	}
	v := r.eventually(t, "x still bound", r.bound)
	if slices.Contains(v.listeners, "agent_y_model") {
		t.Error("y got listeners in x's namespace")
	}
}

// TestSVIDAfterBound: in Broker mode, the identity of an agent whose
// Source waits for its bind (the OCI hook, which holds the container's
// start, and with it the Podman API SPIRE attests through) is subscribed
// only once Envoy reported its listeners and the Source was told; one with
// no wait (read back after a restart) is subscribed at once.
func TestSVIDAfterBound(t *testing.T) {
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour, SVIDAfterBound: true})
	sel := r.sel.Type + ":" + r.sel.Value
	bound := make(chan error, 1)
	r.src.events <- agent.Event{Agent: r.x, Bound: bound}
	v := r.eventually(t, "x's listeners", func(v view) bool { return slices.Contains(v.listeners, "agent_x_model") })
	time.Sleep(50 * time.Millisecond)
	if n := r.fake.Opened(sel); n != 0 {
		t.Fatalf("%d SVID subscriptions before Envoy bound the listeners, want none", n)
	}
	r.ctl.ListenersApplied(v.snap.GetVersion(resource.ListenerType))
	if err := recv(t, bound); err != nil {
		t.Fatalf("Bound = %v", err)
	}
	waitStreams(t, r.fake, sel, 1)
	r.eventually(t, "x bound, its SVID served", r.bound)

	r.src.events <- agent.Event{Agent: r.x, Removed: true}
	waitStreams(t, r.fake, sel, 0)
	again := r.x
	// Elsewhere: no rebind at the old path to wait for.
	again.Netns, again.NetnsID.Ino = "/run/netns/redoma-x-again", again.NetnsID.Ino+1
	r.src.events <- agent.Event{Agent: again}
	waitStreams(t, r.fake, sel, 1)
}
