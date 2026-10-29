package hooksource

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/test/bufconn"

	"github.com/bpalermo/redoma/common/register"
	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/identity/identitytest"
)

var (
	cidA = strings.Repeat("a", 64)
	cidB = strings.Repeat("b", 64)
)

// fakeStat maps a path to its namespace.
type fakeStat struct {
	mu  sync.Mutex
	ids map[string]agent.NetnsID
}

func (f *fakeStat) set(path string, id agent.NetnsID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[path] = id
}

func (f *fakeStat) remove(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ids, path)
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

// env is a Source on an in-memory socket, its events, and a fake namespace
// table. uid is the peer every connection appears to come from.
type env struct {
	src    *Source
	l      *bufconn.Listener
	events chan agent.Event
	stat   *fakeStat
	uid    uint32
	mu     sync.Mutex
	stop   func()
}

func newEnv(t *testing.T, state string) *env {
	t.Helper()
	return newEnvWith(t, state, nil)
}

// newEnvWith is newEnv with change applied to the Source before it runs.
func newEnvWith(t *testing.T, state string, change func(*Source)) *env {
	t.Helper()
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc")
	if err := os.Mkdir(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	allow := filepath.Join(etc, "agents.txt")
	// The launcher's format: whole lines. The comment and the indented and
	// annotated lines allow nothing.
	if err := os.WriteFile(allow, []byte("# who may run\nbeta\nalpha\n  indented\nannotated # note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state == "" {
		state = filepath.Join(dir, "registrations.json")
	}
	e := &env{
		l:      bufconn.Listen(1 << 16),
		events: make(chan agent.Event, 32),
		stat:   &fakeStat{ids: map[string]agent.NetnsID{}},
	}
	e.src = &Source{
		Listener:       e.l,
		Allowlist:      allow,
		AllowlistOwner: uint32(os.Getuid()),
		AllowlistRoot:  dir,
		NetnsDir:       "/run/netns",
		StateFile:      state,
		Interval:       5 * time.Millisecond,
		BoundTimeout:   200 * time.Millisecond,
		Log:            slog.New(slog.DiscardHandler),
		PeerUID: func(net.Conn) (uint32, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.uid, nil
		},
		Stat: e.stat.stat,
	}
	if change != nil {
		change(e.src)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.src.Run(ctx, func(ev agent.Event) { e.events <- ev }) }()
	var once sync.Once
	e.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(e.stop)
	if ev := e.next(t); !ev.Synced {
		t.Fatalf("first event %+v, want Synced", ev)
	}
	return e
}

func (e *env) next(t *testing.T) agent.Event {
	t.Helper()
	select {
	case ev := <-e.events:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
		panic("unreachable")
	}
}

func (e *env) none(t *testing.T) {
	t.Helper()
	select {
	case ev := <-e.events:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(30 * time.Millisecond):
	}
}

// call sends req as the hook does, asynchronously, and returns where its
// answer arrives.
func (e *env) call(t *testing.T, req register.Request) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		conn, err := e.l.Dial()
		if err != nil {
			out <- "dial: " + err.Error()
			return
		}
		defer conn.Close()
		// As the hook does: an answer sent before the request was read still
		// counts when the write fails.
		werr := register.Write(conn, req)
		var resp register.Response
		if err := register.Read(conn, &resp); err != nil {
			if werr != nil {
				out <- "write: " + werr.Error()
				return
			}
			out <- "read: " + err.Error()
			return
		}
		out <- resp.Error
	}()
	return out
}

func answer(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no answer")
		return ""
	}
}

func regReq(container, name string) register.Request {
	return register.Request{Op: register.OpRegister, Container: container, Agent: name, Netns: register.NetnsPath("/run/netns", container)}
}

// registered registers name for container, confirming the bind, and
// returns the Added event.
func (e *env) registered(t *testing.T, container, name string, ino uint64) agent.Event {
	t.Helper()
	e.stat.set(register.NetnsPath("/run/netns", container), agent.NetnsID{Dev: 4, Ino: ino})
	reply := e.call(t, regReq(container, name))
	ev := e.next(t)
	if ev.Removed || ev.Agent.ID != name || ev.Bound == nil {
		t.Fatalf("event %+v, want %s started with a Bound", ev, name)
	}
	ev.Bound <- nil
	if got := answer(t, reply); got != "" {
		t.Fatalf("register: %s", got)
	}
	return ev
}

func TestRegisterWaitsForTheBind(t *testing.T) {
	e := newEnv(t, "")
	e.stat.set(register.NetnsPath("/run/netns", cidA), agent.NetnsID{Dev: 4, Ino: 7})
	reply := e.call(t, regReq(cidA, "beta"))
	ev := e.next(t)
	want := agent.Agent{
		ID:        "beta",
		Netns:     "/run/netns/redoma-oci-" + cidA,
		NetnsID:   agent.NetnsID{Dev: 4, Ino: 7},
		Selectors: Selectors("beta"),
	}
	if !ev.Agent.Equal(want) || ev.Bound == nil {
		t.Fatalf("event %+v, want %+v with a Bound", ev, want)
	}
	select {
	case got := <-reply:
		t.Fatalf("answered %q before the bind", got)
	case <-time.After(30 * time.Millisecond):
	}
	ev.Bound <- nil
	if got := answer(t, reply); got != "" {
		t.Errorf("register: %s", got)
	}
}

func TestRegisterRefusals(t *testing.T) {
	e := newEnv(t, "")
	e.stat.set(register.NetnsPath("/run/netns", cidA), agent.NetnsID{Dev: 4, Ino: 7})
	for name, req := range map[string]register.Request{
		"not allowlisted": regReq(cidA, "someone-else"),
		"indented line":   regReq(cidA, "indented"),
		"annotated line":  regReq(cidA, "annotated"),
		"not a name":      regReq(cidA, "Bad_Name"),
		"not a container": regReq("../../etc", "beta"),
		"another path": {
			Op: register.OpRegister, Container: cidA, Agent: "beta", Netns: "/proc/1/ns/net",
		},
		"no namespace": regReq(cidB, "beta"),
		"unknown op":   {Op: "steal", Container: cidA},
	} {
		if got := answer(t, e.call(t, req)); got == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	e.none(t)
}

// TestAllowlistMustBeTheOwners: an allowlist another user could have
// written allows no name, since the name is the identity.
func TestAllowlistMustBeTheOwners(t *testing.T) {
	e := newEnv(t, "")
	e.stat.set(register.NetnsPath("/run/netns", cidA), agent.NetnsID{Dev: 4, Ino: 7})
	if err := os.Chmod(e.src.Allowlist, 0o664); err != nil {
		t.Fatal(err)
	}
	if got := answer(t, e.call(t, regReq(cidA, "beta"))); !strings.Contains(got, "writable by it alone") {
		t.Errorf("group-writable allowlist: %q, want refused", got)
	}
	e.none(t)

	other := newEnvWith(t, "", func(s *Source) { s.AllowlistOwner = uint32(os.Getuid()) + 1 })
	other.stat.set(register.NetnsPath("/run/netns", cidA), agent.NetnsID{Dev: 4, Ino: 7})
	if got := answer(t, other.call(t, regReq(cidA, "beta"))); !strings.Contains(got, "must be owned by") {
		t.Errorf("allowlist of another owner: %q, want refused", got)
	}
	other.none(t)
}

// TestAllowlistCarriageReturn: a carriage return anywhere in the allowlist
// refuses every name, since the launcher's grep and this reader would not
// agree on such a line.
func TestAllowlistCarriageReturn(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		allowed bool
	}{
		"LF":                   {"beta\nalpha\n", true},
		"no final newline":     {"beta", true},
		"CRLF":                 {"beta\r\n", false},
		"CRLF on another line": {"alpha\r\nbeta\n", false},
		"CR at the end":        {"beta\n\r", false},
		"name not on a line":   {"betas\n", false},
	} {
		e := newEnv(t, "")
		if err := os.WriteFile(e.src.Allowlist, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := e.src.allowed("beta")
		if got != tc.allowed {
			t.Errorf("%s: allowed = %v (%v), want %v", name, got, err, tc.allowed)
		}
	}
}

// TestAllowlistPathMustBeTrusted: a symlinked allowlist, and one under a
// directory another user could write, allow no name.
func TestAllowlistPathMustBeTrusted(t *testing.T) {
	e := newEnv(t, "")
	e.stat.set(register.NetnsPath("/run/netns", cidA), agent.NetnsID{Dev: 4, Ino: 7})
	real := e.src.Allowlist + ".real"
	if err := os.Rename(e.src.Allowlist, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, e.src.Allowlist); err != nil {
		t.Fatal(err)
	}
	if got := answer(t, e.call(t, regReq(cidA, "beta"))); !strings.Contains(got, "not a symlink") {
		t.Errorf("symlinked allowlist: %q, want refused", got)
	}
	if err := os.Remove(e.src.Allowlist); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(real, e.src.Allowlist); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(e.src.Allowlist)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if got := answer(t, e.call(t, regReq(cidA, "beta"))); !strings.Contains(got, dir+" must be owned") {
		t.Errorf("world-writable directory: %q, want refused", got)
	}
	e.none(t)
}

// TestOnlyRoot: a registration from any uid but 0 is refused, before the
// request is even read.
func TestOnlyRoot(t *testing.T) {
	e := newEnv(t, "")
	e.mu.Lock()
	e.uid = 1000
	e.mu.Unlock()
	e.stat.set(register.NetnsPath("/run/netns", cidA), agent.NetnsID{Dev: 4, Ino: 7})
	if got := answer(t, e.call(t, regReq(cidA, "beta"))); !strings.Contains(got, "only root") {
		t.Errorf("uid 1000 got %q, want refused", got)
	}
	e.none(t)
}

// TestFailedBindIsWithdrawn: a register whose bind fails, or times out, is
// answered with the error and withdrawn.
func TestFailedBindIsWithdrawn(t *testing.T) {
	e := newEnv(t, "")
	e.stat.set(register.NetnsPath("/run/netns", cidA), agent.NetnsID{Dev: 4, Ino: 7})
	reply := e.call(t, regReq(cidA, "beta"))
	e.next(t).Bound <- errors.New("replaced")
	if got := answer(t, reply); !strings.Contains(got, "replaced") {
		t.Errorf("register = %q, want the bind's error", got)
	}
	if ev := e.next(t); !ev.Removed {
		t.Errorf("event %+v, want the claim withdrawn", ev)
	}

	reply = e.call(t, regReq(cidA, "beta"))
	e.next(t) // no answer on Bound: BoundTimeout
	if got := answer(t, reply); !strings.Contains(got, "did not bind") {
		t.Errorf("register = %q, want a timeout", got)
	}
	if ev := e.next(t); !ev.Removed {
		t.Errorf("event %+v, want the claim withdrawn", ev)
	}
}

// TestOneContainerPerName: a name held by a live container is refused to
// another; held by one whose namespace is gone, it moves.
func TestOneContainerPerName(t *testing.T) {
	e := newEnv(t, "")
	e.registered(t, cidA, "beta", 7)

	e.stat.set(register.NetnsPath("/run/netns", cidB), agent.NetnsID{Dev: 4, Ino: 8})
	if got := answer(t, e.call(t, regReq(cidB, "beta"))); !strings.Contains(got, "registered by container "+cidA) {
		t.Errorf("second claim got %q, want refused", got)
	}
	e.none(t)

	e.stat.remove(register.NetnsPath("/run/netns", cidA))
	// The rescan may get there first; either way the name moves to B.
	reply := e.call(t, regReq(cidB, "beta"))
	for {
		ev := e.next(t)
		if ev.Removed {
			continue
		}
		if ev.Agent.Netns != register.NetnsPath("/run/netns", cidB) {
			t.Fatalf("event %+v, want B's namespace", ev)
		}
		ev.Bound <- nil
		break
	}
	if got := answer(t, reply); got != "" {
		t.Errorf("register: %s", got)
	}
}

func TestUnregister(t *testing.T) {
	e := newEnv(t, "")
	e.registered(t, cidA, "beta", 7)
	for range 2 { // idempotent
		if got := answer(t, e.call(t, register.Request{Op: register.OpUnregister, Container: cidA})); got != "" {
			t.Fatalf("unregister: %s", got)
		}
	}
	if ev := e.next(t); !ev.Removed || ev.Agent.ID != "beta" {
		t.Errorf("event %+v, want beta removed", ev)
	}
	e.none(t)
}

// TestRescanRemovesAGoneNamespace: poststop never came (a crash), and the
// namespace was unmounted or replaced: the registration goes.
func TestRescanRemovesAGoneNamespace(t *testing.T) {
	e := newEnv(t, "")
	e.registered(t, cidA, "beta", 7)
	e.stat.set(register.NetnsPath("/run/netns", cidA), agent.NetnsID{Dev: 4, Ino: 99})
	if ev := e.next(t); !ev.Removed {
		t.Errorf("event %+v, want removed", ev)
	}
}

// TestRegistrationsSurviveARestart: a restart of redoma-xds emits the
// recorded registrations whose namespace is still there, then Synced.
func TestRegistrationsSurviveARestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "registrations.json")
	e := newEnv(t, state)
	e.registered(t, cidA, "beta", 7)
	e.registered(t, cidB, "alpha", 8)
	e.stop()

	e2 := &env{l: bufconn.Listen(1 << 16), events: make(chan agent.Event, 32), stat: e.stat}
	e.stat.remove(register.NetnsPath("/run/netns", cidB))
	e2.src = &Source{
		Listener: e2.l, Allowlist: e.src.Allowlist, AllowlistOwner: uint32(os.Getuid()), AllowlistRoot: e.src.AllowlistRoot, NetnsDir: "/run/netns", StateFile: state,
		Interval: time.Hour, BoundTimeout: time.Second, Log: slog.New(slog.DiscardHandler),
		PeerUID: func(net.Conn) (uint32, error) { return 0, nil }, Stat: e.stat.stat,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = e2.src.Run(ctx, func(ev agent.Event) { e2.events <- ev }) }()
	if ev := e2.next(t); ev.Synced || ev.Agent.ID != "beta" {
		t.Fatalf("first event %+v, want beta, recorded and still there", ev)
	}
	if ev := e2.next(t); !ev.Synced {
		t.Fatalf("second event %+v, want Synced (alpha's namespace is gone)", ev)
	}
}

// TestPeerUIDOnARealSocket: Listen makes a 0600 socket, and PeerUID reads
// the connecting process's uid from SO_PEERCRED. Skipped where unix sockets
// are refused (failed instead under CI).
func TestPeerUIDOnARealSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "redoma-")
	if err != nil {
		dir = t.TempDir()
	} else {
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
	path := filepath.Join(dir, "register.sock")
	l, err := Listen(path)
	identitytest.SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, %v; want 0600", fi.Mode(), err)
	}
	go func() {
		if c, err := net.Dial("unix", path); err == nil {
			defer c.Close()
			time.Sleep(100 * time.Millisecond)
		}
	}()
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	uid, err := PeerUID(conn)
	if err != nil {
		t.Fatal(err)
	}
	if uid != uint32(os.Getuid()) {
		t.Errorf("PeerUID = %d, want %d", uid, os.Getuid())
	}
}

// TestSharedNamespaceBetweenContainers: a second container in the first's
// namespace (--network container:, a pod) is refused, under any name.
func TestSharedNamespaceBetweenContainers(t *testing.T) {
	e := newEnv(t, "")
	e.registered(t, cidA, "beta", 7)
	e.stat.set(register.NetnsPath("/run/netns", cidB), agent.NetnsID{Dev: 4, Ino: 7})
	if got := answer(t, e.call(t, regReq(cidB, "alpha"))); !strings.Contains(got, "is container "+cidA+"'s") {
		t.Errorf("shared namespace: %q, want refused", got)
	}
	e.none(t)
}
