package hooksource

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bpalermo/redoma/common/pidfd"
	"github.com/bpalermo/redoma/common/register"
	"github.com/bpalermo/redoma/xds/internal/agent"
)

// TestMain is also the process the pidfd tests hold: with
// REDOMA_HOOKSOURCE_CHILD set it runs until its stdin closes.
func TestMain(m *testing.M) {
	if os.Getenv("REDOMA_HOOKSOURCE_CHILD") != "" {
		_, _ = os.Stdin.Read(make([]byte, 1))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// child starts a process and returns a pidfd file for it, and stop, which
// ends and reaps it.
func child(t *testing.T) (*os.File, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "REDOMA_HOOKSOURCE_CHILD=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			in.Close()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(stop)
	f, err := pidfd.Open(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return f, stop
}

// fakeStore records what is kept in systemd's store.
type fakeStore struct {
	mu   sync.Mutex
	ops  []string
	kept map[string]int // name -> descriptors
}

func (f *fakeStore) Add(name string, _ *os.File) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "add "+name)
	f.kept[name]++
	return nil
}

func (f *fakeStore) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "remove "+name)
	delete(f.kept, name)
	return nil
}

func (f *fakeStore) held(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kept[name]
}

// withPidfds has the Source read a register for a container in files as if
// that file came with it (bufconn passes no descriptors), and keep pidfds
// in store.
func withPidfds(store *fakeStore, files map[string]*os.File, mu *sync.Mutex) func(*Source) {
	return func(s *Source) {
		s.Store = store
		s.ReadRequest = func(conn net.Conn) (register.Request, *os.File, error) {
			var req register.Request
			if err := register.Read(conn, &req); err != nil {
				return req, nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			f := files[req.Container]
			delete(files, req.Container)
			return req, f, nil
		}
	}
}

// TestRegisterKeepsThePidfd: the agent's Process is the pidfd that came
// with its register, kept in the store under the container's ID, after
// anything an earlier run left there; it is closed and removed with the
// registration.
func TestRegisterKeepsThePidfd(t *testing.T) {
	f, stop := child(t)
	store := &fakeStore{kept: map[string]int{}}
	var mu sync.Mutex
	e := newEnvWith(t, "", withPidfds(store, map[string]*os.File{cidA: f}, &mu))
	ev := e.registered(t, cidA, "beta", 7)
	if ev.Agent.Process == nil || !ev.Agent.Process.Alive() {
		t.Fatalf("agent %+v, want its live process", ev.Agent)
	}
	if got := strings.Join(store.ops, ","); got != "remove "+cidA+",add "+cidA {
		t.Errorf("store ops %s, want remove then add", got)
	}
	stop()
	if ev.Agent.Process.Alive() {
		t.Error("the process exited and its pidfd says alive")
	}
	if got := answer(t, e.call(t, register.Request{Op: register.OpUnregister, Container: cidA})); got != "" {
		t.Fatal(got)
	}
	if ev := e.next(t); !ev.Removed {
		t.Fatalf("event %+v, want removed", ev)
	}
	if store.held(cidA) != 0 {
		t.Error("the unregistered container's pidfd is still in the store")
	}
	if _, err := ev.Agent.Process.PID(); err == nil {
		t.Error("the unregistered container's pidfd is still open")
	}
}

// TestRegisterRefusesABadPidfd: a descriptor that is no pidfd, or the
// pidfd of a process that exited, refuses the register.
func TestRegisterRefusesABadPidfd(t *testing.T) {
	dead, stop := child(t)
	stop()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	store := &fakeStore{kept: map[string]int{}}
	var mu sync.Mutex
	e := newEnvWith(t, "", withPidfds(store, map[string]*os.File{cidA: r, cidB: dead}, &mu))
	for container, want := range map[string]string{cidA: "not a pidfd", cidB: "has exited"} {
		e.stat.set(register.NetnsPath("/run/netns", container), agent.NetnsID{Dev: 4, Ino: 7})
		if got := answer(t, e.call(t, regReq(container, "beta"))); !strings.Contains(got, want) {
			t.Errorf("register %s: %q, want %q", container[:1], got, want)
		}
	}
	e.none(t)
	if len(store.ops) != 0 {
		t.Errorf("store ops %v for refused registers", store.ops)
	}
}

// TestRestartUnderTheSameID: a container started again under the same ID
// registers again: the new run's pidfd replaces the old one, which is
// closed, and the store holds only the new one.
func TestRestartUnderTheSameID(t *testing.T) {
	first, stopFirst := child(t)
	second, _ := child(t)
	store := &fakeStore{kept: map[string]int{}}
	var mu sync.Mutex
	files := map[string]*os.File{cidA: first}
	e := newEnvWith(t, "", withPidfds(store, files, &mu))
	old := e.registered(t, cidA, "beta", 7)
	stopFirst()
	mu.Lock()
	files[cidA] = second
	mu.Unlock()
	// No poststop came for the first run (it can come late, or never). The
	// namespace is left as recorded: a new one at the same path would race the
	// rescan, which drops a registration whose namespace changed.
	renewed := e.registered(t, cidA, "beta", 7)
	if renewed.Agent.Process == nil || !renewed.Agent.Process.Alive() {
		t.Fatalf("agent %+v, want the second run's live process", renewed.Agent)
	}
	if _, err := old.Agent.Process.PID(); err == nil {
		t.Error("the first run's pidfd is still open")
	}
	if store.held(cidA) != 1 {
		t.Errorf("the store holds %d descriptors for the container, want 1", store.held(cidA))
	}
	want := []string{"remove " + cidA, "add " + cidA, "remove " + cidA, "add " + cidA}
	if !slices.Equal(store.ops, want) {
		t.Errorf("store ops %v, want %v", store.ops, want)
	}
}

// TestPidfdsSurviveARestart: after a restart of redoma xds, a recorded
// registration gets back the pidfd stored under its container's ID when
// exactly one stored there is live; several live ones give it none; an
// exited one is dropped; one with no registration is closed. The store
// then holds exactly the registrations' pidfds.
func TestPidfdsSurviveARestart(t *testing.T) {
	cidC, cidD := strings.Repeat("c", 64), strings.Repeat("d", 64)
	state := filepath.Join(t.TempDir(), "registrations.json")
	e := newEnv(t, state)
	if err := os.WriteFile(e.src.Allowlist, []byte("beta\nalpha\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.registered(t, cidA, "beta", 7)
	e.registered(t, cidB, "alpha", 8)
	e.registered(t, cidC, "gamma", 9)
	e.stop()

	live := func() *os.File { f, _ := child(t); return f }
	exited := func() *os.File { f, stop := child(t); stop(); return f }
	stray := live()
	store := &fakeStore{kept: map[string]int{}}
	stored := map[string][]*os.File{
		cidA: {exited(), live()}, // the old run's exited: the new run's kept
		cidB: {live(), live()},   // two live: ambiguous, none kept
		cidD: {stray},            // no registration
		cidC: {exited()},         // its process is gone
	}
	e2 := newEnvWith(t, state, func(s *Source) {
		s.Allowlist = e.src.Allowlist
		s.AllowlistRoot = e.src.AllowlistRoot
		s.Stat = e.stat.stat
		s.Store = store
		s.Stored = stored
	})
	got := map[string]bool{}
	for _, ev := range e2.drained {
		got[ev.Agent.ID] = ev.Agent.Process != nil && ev.Agent.Process.Alive()
	}
	if want := map[string]bool{"beta": true, "alpha": false, "gamma": false}; !maps(got, want) {
		t.Errorf("agents with a live process: %v, want %v", got, want)
	}
	if store.held(cidA) != 1 || store.held(cidB) != 0 || store.held(cidC) != 0 || store.held(cidD) != 0 {
		t.Errorf("store holds %v, want only %s", store.kept, cidA[:1])
	}
	if _, err := stray.Stat(); err == nil {
		t.Error("the stray pidfd is still open")
	}
}

func maps(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestRegisterWithoutAPidfd: a register the hook could not bind a process
// for is accepted, its agent without a Process (which in Broker mode gets
// it no SVID: identity's TestBrokerWithoutAProcess), and nothing is stored.
func TestRegisterWithoutAPidfd(t *testing.T) {
	store := &fakeStore{kept: map[string]int{}}
	var mu sync.Mutex
	e := newEnvWith(t, "", withPidfds(store, map[string]*os.File{}, &mu))
	ev := e.registered(t, cidA, "beta", 7)
	if ev.Agent.Process != nil {
		t.Errorf("agent %+v, want no process", ev.Agent)
	}
	if store.held(cidA) != 0 {
		t.Error("something was stored for a register without a pidfd")
	}
}

// TestReRegisterWithoutAPidfd: a container registered again under the same
// ID without a pidfd (the hook could not bind its new process) loses the
// old run's: it is closed, and removed from the store, so a restart of
// redoma xds cannot give the old run's pidfd to the new run.
func TestReRegisterWithoutAPidfd(t *testing.T) {
	f, _ := child(t)
	store := &fakeStore{kept: map[string]int{}}
	var mu sync.Mutex
	e := newEnvWith(t, "", withPidfds(store, map[string]*os.File{cidA: f}, &mu))
	old := e.registered(t, cidA, "beta", 7)
	renewed := e.registered(t, cidA, "beta", 7)
	if renewed.Agent.Process != nil {
		t.Errorf("agent %+v, want no process", renewed.Agent)
	}
	if _, err := old.Agent.Process.PID(); err == nil {
		t.Error("the first run's pidfd is still open")
	}
	if store.held(cidA) != 0 {
		t.Errorf("the store holds %d descriptors for the container, want none", store.held(cidA))
	}
	want := []string{"remove " + cidA, "add " + cidA, "remove " + cidA}
	if !slices.Equal(store.ops, want) {
		t.Errorf("store ops %v, want %v", store.ops, want)
	}
}
