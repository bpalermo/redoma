package activedir

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bpalermo/redoma/xds/internal/agent"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func ids(agents []agent.Agent) []string {
	var out []string
	for _, a := range agents {
		out = append(out, a.ID)
	}
	return out
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	active, netns := filepath.Join(dir, "active"), filepath.Join(dir, "netns")
	touch(t, filepath.Join(active, "beta"))
	touch(t, filepath.Join(netns, "redoma-beta"))
	touch(t, filepath.Join(active, "alpha"))
	touch(t, filepath.Join(netns, "redoma-alpha"))
	// Starting: no namespace yet.
	touch(t, filepath.Join(active, "starting"))
	// A namespace with no active file: stopped, or another tool's.
	touch(t, filepath.Join(netns, "redoma-stopped"))
	touch(t, filepath.Join(active, "Not_A_Name"))
	touch(t, filepath.Join(active, ".hidden"))

	running, invalid, err := Scan(active, netns, StatNetns)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "beta"}; !slices.Equal(ids(running), want) {
		t.Errorf("running = %q, want %q", ids(running), want)
	}
	if want := []string{".hidden", "Not_A_Name"}; !slices.Equal(invalid, want) {
		t.Errorf("invalid = %q, want %q", invalid, want)
	}
	nsID, err := StatNetns(filepath.Join(netns, "redoma-beta"))
	if err != nil {
		t.Fatal(err)
	}
	if nsID == (agent.NetnsID{}) {
		t.Error("StatNetns gave no device and inode")
	}
	want := agent.Agent{
		ID:        "beta",
		Netns:     filepath.Join(netns, "redoma-beta"),
		NetnsID:   nsID,
		Selectors: []agent.Selector{{Type: "redoma", Value: "agent:beta"}},
	}
	if !running[1].Equal(want) {
		t.Errorf("running[1] = %+v, want %+v", running[1], want)
	}
}

func TestScanWithoutActiveDirIsNoAgents(t *testing.T) {
	running, invalid, err := Scan(filepath.Join(t.TempDir(), "missing"), t.TempDir(), StatNetns)
	if err != nil || running != nil || invalid != nil {
		t.Errorf("Scan = %v, %q, %v; want nothing", running, invalid, err)
	}
}

func TestValidName(t *testing.T) {
	for repo, want := range map[string]bool{
		"beta":      true,
		"a":         true,
		"0x":        true,
		"-leading":  false,
		"Upper":     false,
		"has.dot":   false,
		"has/slash": false,
		"":          false,
		"a2345678901234567890123456789012345678901":  true,
		"a23456789012345678901234567890123456789012": false,
	} {
		if got := ValidName(repo); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", repo, got, want)
		}
	}
}

// TestSourceEmitsStartsAndStops: the running agents at start, then each
// start and stop once.
func TestSourceEmitsStartsAndStops(t *testing.T) {
	dir := t.TempDir()
	s := &Source{
		ActiveDir: filepath.Join(dir, "active"),
		NetnsDir:  filepath.Join(dir, "netns"),
		Interval:  5 * time.Millisecond,
		Log:       slog.New(slog.DiscardHandler),
	}
	touch(t, filepath.Join(s.ActiveDir, "a"))
	touch(t, Namespace(s.NetnsDir, "a"))

	events := make(chan agent.Event, 16)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, func(e agent.Event) { events <- e }) }()
	next := func() agent.Event {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for an event")
			panic("unreachable")
		}
	}

	if e := next(); e.Removed || e.Agent.ID != "a" || e.Agent.Netns != Namespace(s.NetnsDir, "a") {
		t.Errorf("first event %+v, want a started", e)
	}
	if e := next(); !e.Synced {
		t.Errorf("second event %+v, want Synced after the initial set", e)
	}
	touch(t, filepath.Join(s.ActiveDir, "b"))
	touch(t, Namespace(s.NetnsDir, "b"))
	if e := next(); e.Removed || e.Agent.ID != "b" {
		t.Errorf("event %+v, want b started", e)
	}
	if err := os.Remove(filepath.Join(s.ActiveDir, "a")); err != nil {
		t.Fatal(err)
	}
	if e := next(); !e.Removed || e.Agent.ID != "a" {
		t.Errorf("event %+v, want a stopped", e)
	}

	// Nothing else: an unchanged state emits nothing.
	select {
	case e := <-events:
		t.Errorf("unexpected event %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

// fakeStat is a StatFunc over a map, so a test can replace the namespace at a
// path without creating one.
type fakeStat struct {
	mu  sync.Mutex
	ids map[string]agent.NetnsID
}

func (f *fakeStat) set(path string, id agent.NetnsID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[path] = id
}

func (f *fakeStat) stat(path string) (agent.NetnsID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.ids[path]
	if !ok {
		return agent.NetnsID{}, fs.ErrNotExist
	}
	return id, nil
}

// TestSourceEmitsAReplacedNamespace: the namespace at an agent's path
// replaced between two scans (an agent restarted with the active file and the
// path unchanged) is a start of the agent in the new namespace.
func TestSourceEmitsAReplacedNamespace(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeStat{ids: map[string]agent.NetnsID{}}
	s := &Source{
		ActiveDir: filepath.Join(dir, "active"),
		NetnsDir:  filepath.Join(dir, "netns"),
		Interval:  5 * time.Millisecond,
		Log:       slog.New(slog.DiscardHandler),
		Stat:      fake.stat,
	}
	touch(t, filepath.Join(s.ActiveDir, "a"))
	ns := Namespace(s.NetnsDir, "a")
	fake.set(ns, agent.NetnsID{Dev: 4, Ino: 100})

	events := make(chan agent.Event, 16)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = s.Run(ctx, func(e agent.Event) { events <- e }) }()
	next := func() agent.Event {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for an event")
			panic("unreachable")
		}
	}

	if e := next(); e.Removed || e.Agent.NetnsID.Ino != 100 {
		t.Fatalf("first event %+v, want a started in namespace 100", e)
	}
	if e := next(); !e.Synced {
		t.Fatalf("second event %+v, want Synced", e)
	}
	fake.set(ns, agent.NetnsID{Dev: 4, Ino: 200})
	e := next()
	if e.Removed || e.Agent.ID != "a" || e.Agent.Netns != ns || e.Agent.NetnsID.Ino != 200 {
		t.Errorf("event %+v, want a started again at the same path in namespace 200", e)
	}
}
