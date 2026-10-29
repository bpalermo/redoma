package controlplane

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/identity"
	"github.com/bpalermo/redoma/xds/internal/identity/identitytest"
)

func readState(t *testing.T, path string) map[string]agent.NetnsID {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st.Held
}

// runFirst runs a controller that binds x in namespace 100 and records it in
// state, then stops it, as redoma-xds stopping with Envoy keeping x's
// listeners.
func runFirst(t *testing.T, state string) {
	t.Helper()
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour, StateFile: state})
	r.start(t)
	r.stop()
	if got := readState(t, state); got[r.x.Netns] != r.x.NetnsID {
		t.Fatalf("state %v does not record x in %v", got, r.x.NetnsID)
	}
}

// TestNamespaceReplacedWhileDown: a namespace replaced at x's path while
// redoma-xds was down is a replacement for the next process: x stays out of
// its snapshots until Envoy reports one applied.
func TestNamespaceReplacedWhileDown(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	runFirst(t, state)

	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour, StateFile: state})
	r.x.NetnsID.Ino++
	r.src.events <- agent.Event{Agent: r.x}
	first := r.eventually(t, "the first snapshot", func(v view) bool { return v.snap != nil })
	if slices.Contains(first.listeners, "agent_x_model") {
		t.Fatal("x bound in the first snapshot, in Envoy's old socket")
	}
	r.stillRemoved(t, 50*time.Millisecond, "before Envoy reported a snapshot without it")

	r.ctl.ListenersApplied(first.snap.GetVersion(resource.ListenerType))
	r.eventually(t, "x bound again", r.bound)
	r.stop()
	if got := readState(t, state); got[r.x.Netns] != r.x.NetnsID {
		t.Errorf("state %v does not record the new namespace %v", got, r.x.NetnsID)
	}
}

// TestSameNamespaceAfterRestart: x still in the recorded namespace is bound
// in the first snapshot, without a wait: Envoy's listener is the same.
func TestSameNamespaceAfterRestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	runFirst(t, state)

	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour, StateFile: state})
	r.src.events <- agent.Event{Agent: r.x}
	r.eventually(t, "x bound without a report", r.bound)
}

// TestUnreadableStateIsIgnored: a state file that does not parse is logged
// and ignored, and agents are bound at once.
func TestUnreadableStateIsIgnored(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour, StateFile: state})
	r.src.events <- agent.Event{Agent: r.x}
	r.eventually(t, "x bound", r.bound)
}

// TestRecordForgetsAReportedRemoval: once Envoy reports an agent's removal
// applied, its path leaves the record.
func TestRecordForgetsAReportedRemoval(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour, StateFile: state})
	r.start(t)
	r.src.events <- agent.Event{Agent: r.x, Removed: true}
	removed := r.eventually(t, "x removed", func(v view) bool { return !slices.Contains(v.listeners, "agent_x_model") })
	if _, ok := readState(t, state)[r.x.Netns]; !ok {
		t.Error("the path left the record before Envoy reported the removal")
	}
	r.ctl.ListenersApplied(removed.snap.GetVersion(resource.ListenerType))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := readState(t, state)[r.x.Netns]; !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the path is still recorded: %v", readState(t, state))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNothingPublishedBeforeTheInitialSet: until the Source reports its
// initial set, no snapshot is published; a snapshot of a partial set would
// make a connected Envoy drop the listeners of the agents not in it yet.
func TestNothingPublishedBeforeTheInitialSet(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	_, conn := identitytest.Serve(t)
	src := &chanSource{events: make(chan agent.Event), err: make(chan error), noSync: true}
	c := cache.NewSnapshotCache(true, NodeHash{}, nil)
	cfg := Config{EtcDir: t.TempDir(), TrustDomain: td, ScanInterval: 5 * time.Millisecond, SyncTimeout: time.Hour}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- New(cfg, src, identity.New(conn, td, log), c, log).Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	src.events <- agent.Event{Agent: agent.Agent{ID: "x", Netns: "/run/netns/redoma-x"}}
	time.Sleep(50 * time.Millisecond)
	if _, err := c.GetSnapshot(NodeKey); err == nil {
		t.Fatal("a snapshot was published before the initial set")
	}
	src.events <- agent.Event{Synced: true}
	e := &env{cache: c}
	e.eventually(t, "x published", func(v view) bool { return slices.Contains(v.listeners, "agent_x_model") })
}

// syncEnv is a controller fed by a Source that never sends Synced, whose
// sync timeouts the test fires: each timeout the controller asks for
// arrives on timeouts, and the test fires it by sending on it.
type syncEnv struct {
	*env
	src      *chanSource
	timeouts chan chan time.Time
}

func newSyncEnv(t *testing.T, state string) *syncEnv {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	_, conn := identitytest.Serve(t)
	s := &syncEnv{
		env:      &env{cache: cache.NewSnapshotCache(true, NodeHash{}, nil)},
		src:      &chanSource{events: make(chan agent.Event), err: make(chan error), noSync: true},
		timeouts: make(chan chan time.Time, 4),
	}
	cfg := Config{EtcDir: t.TempDir(), TrustDomain: td, ScanInterval: 5 * time.Millisecond, StateFile: state, SyncTimeout: time.Hour}
	ctl := New(cfg, s.src, identity.New(conn, td, log), s.cache, log)
	ctl.after = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		s.timeouts <- ch
		return ch
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ctl.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s
}

// timeout is the next sync timeout the controller asks for.
func (s *syncEnv) timeout(t *testing.T) chan time.Time {
	t.Helper()
	select {
	case ch := <-s.timeouts:
		return ch
	case <-time.After(10 * time.Second):
		t.Fatal("the controller is not waiting on a sync timeout")
		return nil
	}
}

func (s *syncEnv) published() bool {
	_, err := s.cache.GetSnapshot(NodeKey)
	return err == nil
}

// TestSyncTimeoutWithoutARecord: with nothing recorded (a fresh Envoy holds
// nothing), a Source that never syncs gets its current set published when
// the timeout fires, not before.
func TestSyncTimeoutWithoutARecord(t *testing.T) {
	s := newSyncEnv(t, "")
	first := s.timeout(t)
	// Delivered to Run (the Source's send returns once Run has it queued):
	// without Synced and before the timeout, nothing can publish it.
	s.src.events <- agent.Event{Agent: agent.Agent{ID: "x", Netns: "/run/netns/redoma-x"}}
	if s.published() {
		t.Fatal("published before the timeout")
	}
	first <- time.Now()
	s.eventually(t, "x published at the timeout", func(v view) bool { return slices.Contains(v.listeners, "agent_x_model") })
}

// TestSyncTimeoutKeepsWaitingWithARecord: with listeners recorded from the
// previous run, a timeout publishes nothing (Envoy would drop them) and
// waits again; the Source's Synced publishes.
func TestSyncTimeoutKeepsWaitingWithARecord(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	raw, err := json.Marshal(stateFile{Held: map[string]agent.NetnsID{"/run/netns/redoma-x": {Dev: 4, Ino: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSyncEnv(t, state)
	for range 2 {
		s.timeout(t) <- time.Now()
	}
	// A third request means the second timeout has been handled, and it
	// published nothing.
	s.timeout(t)
	if s.published() {
		t.Fatal("published at a timeout while Envoy may hold the previous run's listeners")
	}
	s.src.events <- agent.Event{Agent: agent.Agent{ID: "x", Netns: "/run/netns/redoma-x", NetnsID: agent.NetnsID{Dev: 4, Ino: 100}}}
	s.src.events <- agent.Event{Synced: true}
	s.eventually(t, "x published on Synced", func(v view) bool { return slices.Contains(v.listeners, "agent_x_model") })
}

// TestLoadRemovesStaleTemporaryFiles: a temporary file left by a crash
// between write and rename is removed at start.
func TestLoadRemovesStaleTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	stale := state + ".123456"
	if err := os.WriteFile(stale, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newReplaceEnv(t, Config{RebindTimeout: time.Hour, StateFile: state})
	r.start(t)
	if _, err := os.Stat(stale); err == nil {
		t.Error("the stale temporary file is still there")
	}
}
