package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// scripted waits for after (if set), emits its events, closes emitted (if
// set), then blocks until ctx ends or fail sends.
type scripted struct {
	events  []Event
	fail    chan error
	after   <-chan struct{}
	emitted chan struct{}
}

func (s *scripted) Run(ctx context.Context, emit func(Event)) error {
	if s.after != nil {
		<-s.after
	}
	for _, e := range s.events {
		emit(e)
	}
	if s.emitted != nil {
		close(s.emitted)
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.fail:
		return err
	}
}

func TestMerge(t *testing.T) {
	bound := make(chan error, 1)
	aDone := make(chan struct{})
	a := &scripted{events: []Event{{Agent: Agent{ID: "x", Netns: "/a"}}, {Synced: true}}, fail: make(chan error), emitted: aDone}
	b := &scripted{events: []Event{{Agent: Agent{ID: "x", Netns: "/b"}, Bound: bound}, {Agent: Agent{ID: "y"}}}, fail: make(chan error), after: aDone}

	got := make(chan Event, 16)
	done := make(chan error, 1)
	go func() {
		done <- Merge(slog.New(slog.DiscardHandler), a, b).Run(t.Context(), func(e Event) { got <- e })
	}()

	// a's x, b's y; b's x is refused. Synced waits for b, which never sends
	// it here.
	seen := map[string]string{}
	for range 2 {
		select {
		case e := <-got:
			if e.Synced {
				t.Fatal("Synced before every source sent it")
			}
			seen[e.Agent.ID] = e.Agent.Netns
		case <-time.After(10 * time.Second):
			t.Fatal("timed out")
		}
	}
	if seen["x"] != "/a" || len(seen) != 2 {
		t.Errorf("events %v, want a's x and b's y", seen)
	}
	select {
	case err := <-bound:
		if err == nil {
			t.Error("b's claim of x was confirmed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("b's claim of x was not refused")
	}

	b.fail <- errors.New("boom")
	select {
	case err := <-done:
		if err == nil || err.Error() != "boom" {
			t.Errorf("Run = %v, want b's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end with a failed source")
	}
}

func TestMergeSyncedOnceAll(t *testing.T) {
	a := &scripted{events: []Event{{Synced: true}}, fail: make(chan error)}
	b := &scripted{events: []Event{{Synced: true}}, fail: make(chan error)}
	got := make(chan Event, 4)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = Merge(slog.New(slog.DiscardHandler), a, b).Run(ctx, func(e Event) { got <- e }) }()
	select {
	case e := <-got:
		if !e.Synced {
			t.Fatalf("event %+v, want Synced", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no Synced")
	}
	select {
	case e := <-got:
		t.Fatalf("a second event %+v", e)
	case <-time.After(30 * time.Millisecond):
	}
}

// chanSrc emits what the test sends it, until ctx ends.
type chanSrc chan Event

func (c chanSrc) Run(ctx context.Context, emit func(Event)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-c:
			emit(e)
		}
	}
}

// mergeRig runs Merge(launcher, hook) and records what it passes on.
type mergeRig struct {
	launcher, hook chanSrc
	got            chan Event
}

func newMergeRig(t *testing.T) *mergeRig {
	t.Helper()
	r := &mergeRig{launcher: make(chanSrc), hook: make(chanSrc), got: make(chan Event, 16)}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		_ = Merge(slog.New(slog.DiscardHandler), r.launcher, r.hook).Run(ctx, func(e Event) { r.got <- e })
	}()
	return r
}

// expect fails unless the next passed event is a start in netns, or a
// removal when netns is "".
func (r *mergeRig) expect(t *testing.T, netns string) {
	t.Helper()
	select {
	case e := <-r.got:
		if netns == "" && !e.Removed || netns != "" && (e.Removed || e.Agent.Netns != netns) {
			t.Fatalf("passed %+v, want %q", e, netns)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("nothing passed, want %q", netns)
	}
}

func (r *mergeRig) none(t *testing.T) {
	t.Helper()
	select {
	case e := <-r.got:
		t.Fatalf("passed %+v, want nothing", e)
	case <-time.After(30 * time.Millisecond):
	}
}

func start(netns string) Event { return Event{Agent: Agent{ID: "x", Netns: netns}} }
func stop() Event              { return Event{Agent: Agent{ID: "x"}, Removed: true} }

// TestMergeLauncherWins: the hook holds x, the launcher
// starts x and wins; the hook stopping changes nothing; when the launcher
// stops x, it is removed (the hook's claim is gone too).
func TestMergeLauncherWins(t *testing.T) {
	r := newMergeRig(t)
	r.hook <- start("/hook")
	r.expect(t, "/hook")
	r.launcher <- start("/launcher")
	r.expect(t, "/launcher")
	r.hook <- stop()
	r.none(t)
	r.launcher <- stop()
	r.expect(t, "")
}

// TestMergeReoffersAKeptClaim: the launcher holds x when the hook's claim
// arrives (a restart: the hook's recorded registrations come back without a
// Bound): it is kept, and passed on once the launcher stops x.
func TestMergeReoffersAKeptClaim(t *testing.T) {
	r := newMergeRig(t)
	r.launcher <- start("/launcher")
	r.expect(t, "/launcher")
	r.hook <- start("/hook")
	r.none(t)
	r.launcher <- stop()
	r.expect(t, "/hook")
	r.hook <- stop()
	r.expect(t, "")
}

// TestMergeIsDeterministic: whichever order the two claims arrive in, the
// launcher's is what ends up passed on.
func TestMergeIsDeterministic(t *testing.T) {
	for _, hookFirst := range []bool{true, false} {
		r := newMergeRig(t)
		if hookFirst {
			r.hook <- start("/hook")
			r.expect(t, "/hook")
			r.launcher <- start("/launcher")
		} else {
			r.launcher <- start("/launcher")
			r.expect(t, "/launcher")
			r.hook <- start("/hook")
		}
		if hookFirst {
			r.expect(t, "/launcher")
		}
		r.none(t)
	}
}

// TestMergeRefusesAWaitingRegistrar: a later Source's start with a Bound,
// while the launcher holds x, is refused at once and never offered again.
func TestMergeRefusesAWaitingRegistrar(t *testing.T) {
	r := newMergeRig(t)
	r.launcher <- start("/launcher")
	r.expect(t, "/launcher")
	bound := make(chan error, 1)
	e := start("/hook")
	e.Bound = bound
	r.hook <- e
	select {
	case err := <-bound:
		if err == nil {
			t.Fatal("Bound = nil for a refused claim")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the claim was not refused")
	}
	r.hook <- stop() // the hook withdraws it
	r.none(t)
	r.launcher <- stop()
	r.expect(t, "")
}
