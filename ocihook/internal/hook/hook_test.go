package hook

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bpalermo/redoma/common/register"
)

const cid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeMounter records the mounts.
type fakeMounter struct {
	mu      sync.Mutex
	mounted map[string]int // target -> pid
	failOn  string
}

func (m *fakeMounter) BindNetns(pid int, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == target {
		return errors.New("mount failed")
	}
	m.mounted[target] = pid
	return nil
}

func (m *fakeMounter) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounted, target)
	return nil
}

func (m *fakeMounter) has(target string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mounted[target]
	return ok
}

// fakeCP answers each request with answer(req), recording the requests.
type fakeCP struct {
	mu       sync.Mutex
	requests []register.Request
	answer   func(register.Request) string
	down     bool
}

func (cp *fakeCP) dial(context.Context) (net.Conn, error) {
	cp.mu.Lock()
	down := cp.down
	cp.mu.Unlock()
	if down {
		return nil, errors.New("connection refused")
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		var req register.Request
		if err := register.Read(server, &req); err != nil {
			return
		}
		cp.mu.Lock()
		cp.requests = append(cp.requests, req)
		cp.mu.Unlock()
		_ = register.Write(server, register.Response{Error: cp.answer(req)})
	}()
	return client, nil
}

func (cp *fakeCP) ops() []string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var out []string
	for _, r := range cp.requests {
		out = append(out, r.Op)
	}
	return out
}

func newHook(cp *fakeCP, m *fakeMounter) (*Hook, *bytes.Buffer) {
	var log bytes.Buffer
	return &Hook{
		Annotation: "redoma/agent",
		NetnsDir:   "/run/netns",
		Timeout:    time.Second,
		Mount:      m,
		Dial:       cp.dial,
		Log:        &log,
		// No mounts, on the host or in the container: the mountinfo
		// checks have their own tests.
		Mountinfo: func(string) ([]mountEntry, error) { return nil, nil },
		// The forbidden paths in the tests' tree (TestMain).
		Forbidden: testForbidden,
		Graphroot: testGraphroot,
		Runroot:   T(Runroot),
	}, &log
}

// bundle writes a bundle whose config.json has namespaces, as JSON.
func bundle(t *testing.T, namespaces string) string {
	t.Helper()
	return writeBundle(t, `{"ociVersion":"1.2.0","root":{"path":"rootfs"},"process":`+agentProcess+`,"linux":{"namespaces":`+namespaces+`}}`)
}

// writeBundle writes a bundle whose config.json is cfg, with the rootfs
// directory a root.path of "rootfs" names, since the root must resolve.
func writeBundle(t *testing.T, cfg string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fresh is the namespaces of a container the runtime makes a new network
// namespace for (--network none).
const fresh = `[{"type":"pid"},{"type":"network"},{"type":"mount"}]`

func state(t *testing.T, annotated bool) State {
	t.Helper()
	st := State{ID: cid, Status: "creating", Pid: 4242, Bundle: bundle(t, fresh), Annotations: map[string]string{"io.podman.annotations.autoremove": "TRUE"}}
	if annotated {
		st.Annotations["redoma/agent"] = "beta"
	}
	return st
}

// TestCreateRuntimeNeedsItsOwnNetns: only a container the runtime makes a
// new network namespace for is registered; the host's (no network
// namespace in the spec) and a joined one (a path) are refused before any
// mount or request.
func TestCreateRuntimeNeedsItsOwnNetns(t *testing.T) {
	for name, tc := range map[string]struct {
		namespaces string
		want       string
	}{
		"--network host":        {`[{"type":"pid"},{"type":"mount"}]`, "--network host"},
		"--network container:x": {`[{"type":"network","path":"/proc/4100/ns/net"}]`, "joins the network namespace /proc/4100/ns/net"},
		"--network ns:agent":    {`[{"type":"network","path":"/run/netns/redoma-foo"}]`, "joins the network namespace /run/netns/redoma-foo"},
		"a pod sibling":         {`[{"type":"network","path":"/run/netns/netns-0c1f"}]`, "joins"},
		"--userns=auto, bridge": {`[{"type":"user"},{"type":"network"}]`, "own user namespace"},
		"--userns with a path":  {`[{"type":"network"},{"type":"user","path":"/proc/77/ns/user"}]`, "own user namespace"},
		"two network entries":   {`[{"type":"network"},{"type":"network","path":"/run/netns/x"}]`, "network namespace twice"},
		"another type twice":    {`[{"type":"network"},{"type":"pid"},{"type":"pid"}]`, "pid namespace twice"},
		"--pid container:x":     {`[{"type":"network"},{"type":"pid","path":"/proc/4100/ns/pid"}]`, "joins the pid namespace"},
		"--ipc container:x":     {`[{"type":"network"},{"type":"ipc","path":"/proc/4100/ns/ipc"}]`, "joins the ipc namespace"},
		"--uts ns:path":         {`[{"type":"network"},{"type":"uts","path":"/run/uts"}]`, "joins the uts namespace"},
		"a mount namespace":     {`[{"type":"network"},{"type":"mount","path":"/proc/1/ns/mnt"}]`, "joins the mount namespace"},
		"--cgroupns container":  {`[{"type":"network"},{"type":"cgroup","path":"/proc/1/ns/cgroup"}]`, "joins the cgroup namespace"},
	} {
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		m := &fakeMounter{mounted: map[string]int{}}
		h, _ := newHook(cp, m)
		st := state(t, true)
		st.Bundle = bundle(t, tc.namespaces)
		err := h.CreateRuntime(t.Context(), st)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: CreateRuntime = %v, want %q", name, err, tc.want)
		}
		if len(m.mounted) != 0 || len(cp.requests) != 0 {
			t.Errorf("%s: mounts %v, requests %v; want none", name, m.mounted, cp.requests)
		}
	}
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	h, _ := newHook(cp, &fakeMounter{mounted: map[string]int{}})
	st := state(t, true)
	st.Bundle = t.TempDir() // no config.json
	if err := h.CreateRuntime(t.Context(), st); err == nil {
		t.Error("a bundle without a spec was accepted")
	}
}

// TestSpecIsReadStrictly: what the runtime would read differently from
// encoding/json is refused or read as the runtime does.
func TestSpecIsReadStrictly(t *testing.T) {
	for name, tc := range map[string]struct {
		config string
		want   string // "" for accepted
	}{
		"--network none": {`{"linux":{"namespaces":[{"type":"network"}]}}`, ""},
		// crun matches keys exactly and runc folds case: a spec key in
		// another case is refused, since the two would read it apart.
		"a key in another case": {`{"Linux":{"namespaces":[{"type":"network"}]}}`, "in another case"},
		"Type in another case":  {`{"linux":{"namespaces":[{"Type":"network"}]}}`, "in another case"},
		"keys only in case":     {`{"linux":{"namespaces":[{"type":"network"}]},"annotations":{"a":"1","A":"2"},"x":{"foo":1,"FOO":2}}`, "differ only in case"},
		// encoding/json folds Unicode too (ſ is s, the Kelvin sign k).
		"additionalGidſ":        {`{"linux":{"namespaces":[{"type":"network"}]},"process":{"user":{"uid":1000,"additionalGid\u017f":[961,0]}}}`, "in another case"},
		"capabilitieſ":          {`{"linux":{"namespaces":[{"type":"network"}]},"process":{"capabilitie\u017f":{"bounding":["CAP_SYS_ADMIN"]}}}`, "in another case"},
		"optionſ":               {`{"linux":{"namespaces":[{"type":"network"}]},"mounts":[{"option\u017f":["rbind"]}]}`, "in another case"},
		"ſource":                {`{"linux":{"namespaces":[{"type":"network"}]},"mounts":[{"\u017fource":"/run/redoma-xds"}]}`, "in another case"},
		"a Kelvin sign":         {`{"linux":{"namespaces":[{"type":"network"}]},"x":{"key":1,"\u212aey":2}}`, "differ only in case"},
		"free annotation keys":  {`{"linux":{"namespaces":[{"type":"network"}]},"annotations":{"Mounts":"x","Process":"y"}}`, ""},
		"a repeated key":        {`{"linux":{"namespaces":[{"type":"network","path":"","path":"/run/netns/x"}]}}`, `"path" appears twice`},
		"a repeated top key":    {`{"linux":{"namespaces":[{"type":"network"}]},"linux":{}}`, `"linux" appears twice`},
		"same key, two objects": {`{"linux":{"namespaces":[{"type":"pid"},{"type":"network"}]}}`, ""},
		"not JSON":              {`{"linux":`, "spec"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.config), 0o644); err != nil {
			t.Fatal(err)
		}
		err := ownNetns(dir)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v, want accepted", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	for _, b := range []string{"relative/bundle", "/abs/../bundle", ""} {
		if err := ownNetns(b); err == nil || !strings.Contains(err.Error(), "clean absolute path") {
			t.Errorf("bundle %q: %v, want refused", b, err)
		}
	}
}

func TestCreateRuntimeRegisters(t *testing.T) {
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	m := &fakeMounter{mounted: map[string]int{}}
	h, _ := newHook(cp, m)
	if err := h.CreateRuntime(t.Context(), state(t, true)); err != nil {
		t.Fatal(err)
	}
	target := "/run/netns/redoma-oci-" + cid
	if m.mounted[target] != 4242 {
		t.Errorf("mounts %v, want pid 4242's namespace at %s", m.mounted, target)
	}
	want := register.Request{Op: register.OpRegister, Container: cid, Agent: "beta", Netns: target}
	if len(cp.requests) != 1 || cp.requests[0] != want {
		t.Errorf("requests %+v, want %+v", cp.requests, want)
	}
}

// TestCreateRuntimeFailureUndoes: a refused register fails the start, and
// the claim and the mount are undone.
func TestCreateRuntimeFailureUndoes(t *testing.T) {
	cp := &fakeCP{answer: func(r register.Request) string {
		if r.Op == register.OpRegister {
			return "Envoy did not bind the listeners within 30s"
		}
		return ""
	}}
	m := &fakeMounter{mounted: map[string]int{}}
	h, _ := newHook(cp, m)
	err := h.CreateRuntime(t.Context(), state(t, true))
	if err == nil || !strings.Contains(err.Error(), "did not bind") {
		t.Fatalf("CreateRuntime = %v, want the control plane's error", err)
	}
	if m.has("/run/netns/redoma-oci-" + cid) {
		t.Error("the namespace is still mounted")
	}
	if got := strings.Join(cp.ops(), ","); got != "register,unregister" {
		t.Errorf("requests %s, want register,unregister", got)
	}
}

// TestCreateRuntimeWithoutControlPlane fails the start: an agent must not
// run without its proxy.
func TestCreateRuntimeWithoutControlPlane(t *testing.T) {
	cp := &fakeCP{down: true}
	m := &fakeMounter{mounted: map[string]int{}}
	h, _ := newHook(cp, m)
	if err := h.CreateRuntime(t.Context(), state(t, true)); err == nil {
		t.Fatal("started with no control plane")
	}
	if m.has("/run/netns/redoma-oci-" + cid) {
		t.Error("the namespace is still mounted")
	}
}

func TestCreateRuntimeIgnoresOtherContainers(t *testing.T) {
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	m := &fakeMounter{mounted: map[string]int{}}
	h, _ := newHook(cp, m)
	if err := h.CreateRuntime(t.Context(), state(t, false)); err != nil {
		t.Fatal(err)
	}
	if len(m.mounted) != 0 || len(cp.requests) != 0 {
		t.Error("a container without the annotation was registered")
	}
}

func TestCreateRuntimeRefusesBadState(t *testing.T) {
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	m := &fakeMounter{mounted: map[string]int{}}
	h, _ := newHook(cp, m)
	bad := state(t, true)
	bad.ID = "../etc"
	if err := h.CreateRuntime(t.Context(), bad); err == nil {
		t.Error("a bad container ID was accepted")
	}
	bad = state(t, true)
	bad.Pid = 0
	if err := h.CreateRuntime(t.Context(), bad); err == nil {
		t.Error("a state without a pid was accepted")
	}
	m.failOn = "/run/netns/redoma-oci-" + cid
	if err := h.CreateRuntime(t.Context(), state(t, true)); err == nil {
		t.Error("a failed mount was not an error")
	}
	if len(cp.requests) != 0 {
		t.Errorf("registered on a bad state: %+v", cp.requests)
	}
}

// TestPoststop unregisters and unmounts, and does both even when the
// control plane is down; it is idempotent.
func TestPoststop(t *testing.T) {
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	m := &fakeMounter{mounted: map[string]int{"/run/netns/redoma-oci-" + cid: 4242}}
	h, _ := newHook(cp, m)
	st := state(t, true)
	st.Status, st.Pid = "stopped", 0
	if err := h.Poststop(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	if m.has("/run/netns/redoma-oci-"+cid) || strings.Join(cp.ops(), ",") != "unregister" {
		t.Errorf("mounts %v, requests %v; want unmounted and unregistered", m.mounted, cp.ops())
	}

	cp.down = true
	m.mounted["/run/netns/redoma-oci-"+cid] = 4242
	h, log := newHook(cp, m)
	if err := h.Poststop(t.Context(), st); err != nil {
		t.Fatalf("poststop with the control plane down: %v", err)
	}
	if m.has("/run/netns/redoma-oci-" + cid) {
		t.Error("not unmounted with the control plane down")
	}
	if !strings.Contains(log.String(), "rescan") {
		t.Errorf("log %q does not say the rescan will clean up", log.String())
	}
	if err := h.Poststop(t.Context(), st); err != nil {
		t.Errorf("a second poststop: %v", err)
	}
}
