package hook

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/common/register"
)

// upstreamNamespaces are an upstream container's under --userns=auto and
// --network none: all new, a user namespace among them.
const upstreamNamespaces = `[{"type":"pid"},{"type":"network"},{"type":"mount"},{"type":"user"}]`

// upstreamProcess is its process: root in its own user namespace, no
// privilege to gain, no capability.
const upstreamProcess = `{"user":{"uid":0,"gid":0},"noNewPrivileges":true,"capabilities":` + noCaps + `}`

const upstreamMappings = `"uidMappings":[{"containerID":0,"hostID":100000,"size":65536}],` +
	`"gidMappings":[{"containerID":0,"hostID":100000,"size":65536}]`

// upstreamHook is a hook with the upstream annotation, the containers
// ranges 100000-165535 (and 200000-200999 for uids), and the tree's token
// root.
func upstreamHook(t *testing.T, cp *fakeCP, m *fakeMounter) *Hook {
	t.Helper()
	h, _ := newHook(cp, m)
	h.UpstreamAnnotation = "redoma/upstream"
	dir := t.TempDir()
	h.SubUID, h.SubGID = filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid")
	if err := os.WriteFile(h.SubUID, []byte("palermo:300000:65536\ncontainers:100000:65536\ncontainers:200000:1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.SubGID, []byte("containers:100000:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.TokenRoot = T("/run/redoma-mcp")
	for _, d := range []string{"grafana", "other"} {
		if err := os.MkdirAll(filepath.Join(h.TokenRoot, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func bindOf(src string) string {
	return `[{"destination":"/run/secrets/x","type":"bind","source":"` + src + `","options":["rbind","ro"]}]`
}

// createUpstream runs createRuntime on an upstream's spec built from its
// parts, and checks that a refusal mounted and asked nothing, and that an
// acceptance mounted at the upstream's path and asked nothing either.
func createUpstream(t *testing.T, namespaces, process, linuxExtra, mounts string, annotations map[string]string) error {
	t.Helper()
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	m := &fakeMounter{mounted: map[string]int{}}
	h := upstreamHook(t, cp, m)
	cfg := `{"ociVersion":"1.2.0","root":{"path":"rootfs"},"process":` + process
	if mounts != "" {
		cfg += `,"mounts":` + mounts
	}
	cfg += `,"linux":{"namespaces":` + namespaces
	if linuxExtra != "" {
		cfg += `,` + linuxExtra
	}
	cfg += `}}`
	st := State{ID: cid, Status: "creating", Pid: 4242, Bundle: writeBundle(t, cfg), Annotations: annotations}
	err := h.CreateRuntime(t.Context(), st)
	if len(cp.requests) != 0 {
		t.Errorf("createRuntime of an upstream asked the control plane %v", cp.requests)
	}
	switch {
	case err != nil && len(m.mounted) != 0:
		t.Errorf("refused (%v), but mounted %v", err, m.mounted)
	case err == nil && !m.has(register.UpstreamNetnsPath("/run/netns", cid)):
		t.Errorf("accepted, but mounted %v", m.mounted)
	}
	return err
}

var upstreamAnnotation = map[string]string{"redoma/upstream": "grafana"}

func TestCreateRuntimeUpstream(t *testing.T) {
	ok := func(name string, err error) { expect(t, name, err, "") }
	ok("its own credential directory", createUpstream(t, upstreamNamespaces, upstreamProcess, upstreamMappings, bindOf(T("/run/redoma-mcp/grafana")), upstreamAnnotation))
	ok("a file in it", createUpstream(t, upstreamNamespaces, upstreamProcess, upstreamMappings, bindOf(T("/run/redoma-mcp/grafana")), upstreamAnnotation))
	ok("no mount", createUpstream(t, upstreamNamespaces, upstreamProcess, upstreamMappings, "", upstreamAnnotation))
	ok("the second uid range", createUpstream(t, upstreamNamespaces, upstreamProcess,
		`"uidMappings":[{"containerID":0,"hostID":200000,"size":1000}],"gidMappings":[{"containerID":0,"hostID":100000,"size":1000}]`, "", upstreamAnnotation))
}

func TestCreateRuntimeUpstreamRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, namespaces, process, linux, mounts string
		annotations                              map[string]string
		want                                     string
	}{
		{"no user namespace", fresh, upstreamProcess, upstreamMappings, "", upstreamAnnotation, "no user namespace"},
		{"a joined network namespace", `[{"type":"pid"},{"type":"network","path":"/run/netns/x"},{"type":"user"}]`, upstreamProcess, upstreamMappings, "", upstreamAnnotation, "joins the network namespace"},
		{"a joined user namespace", `[{"type":"pid"},{"type":"network"},{"type":"user","path":"/proc/1/ns/user"}]`, upstreamProcess, upstreamMappings, "", upstreamAnnotation, "joins the user namespace"},
		{"the host's network", `[{"type":"pid"},{"type":"user"}]`, upstreamProcess, upstreamMappings, "", upstreamAnnotation, "host's network"},
		{"the host's PIDs", `[{"type":"network"},{"type":"user"}]`, upstreamProcess, upstreamMappings, "", upstreamAnnotation, "PID namespace"},
		{"no mappings", upstreamNamespaces, upstreamProcess, "", "", upstreamAnnotation, "no uidMappings"},
		{"host root mapped", upstreamNamespaces, upstreamProcess, `"uidMappings":[{"containerID":0,"hostID":0,"size":1}],"gidMappings":[{"containerID":0,"hostID":100000,"size":1}]`, "", upstreamAnnotation, "outside the containers ranges"},
		{"a service's gid mapped", upstreamNamespaces, upstreamProcess, `"uidMappings":[{"containerID":0,"hostID":100000,"size":1}],"gidMappings":[{"containerID":0,"hostID":961,"size":1}]`, "", upstreamAnnotation, "outside the containers ranges"},
		{"past the range's end", upstreamNamespaces, upstreamProcess, `"uidMappings":[{"containerID":0,"hostID":100000,"size":65537}],"gidMappings":[{"containerID":0,"hostID":100000,"size":1}]`, "", upstreamAnnotation, "outside the containers ranges"},
		{"across two ranges", upstreamNamespaces, upstreamProcess, `"uidMappings":[{"containerID":0,"hostID":165000,"size":1000}],"gidMappings":[{"containerID":0,"hostID":100000,"size":1}]`, "", upstreamAnnotation, "outside the containers ranges"},
		{"another user's range", upstreamNamespaces, upstreamProcess, `"uidMappings":[{"containerID":0,"hostID":300000,"size":10}],"gidMappings":[{"containerID":0,"hostID":100000,"size":1}]`, "", upstreamAnnotation, "outside the containers ranges"},
		{"an empty mapping", upstreamNamespaces, upstreamProcess, `"uidMappings":[{"containerID":0,"hostID":100000,"size":0}],"gidMappings":[{"containerID":0,"hostID":100000,"size":1}]`, "", upstreamAnnotation, "outside the containers ranges"},
		{"privileges to gain", upstreamNamespaces, `{"user":{"uid":0,"gid":0},"noNewPrivileges":false,"capabilities":` + noCaps + `}`, upstreamMappings, "", upstreamAnnotation, "no-new-privileges"},
		{"a capability", upstreamNamespaces, `{"user":{"uid":0,"gid":0},"noNewPrivileges":true,"capabilities":{"bounding":["CAP_SYS_ADMIN"]}}`, upstreamMappings, "", upstreamAnnotation, "CAP_SYS_ADMIN"},
		{"another upstream's credential", upstreamNamespaces, upstreamProcess, upstreamMappings, bindOf(T("/run/redoma-mcp/other")), upstreamAnnotation, "reaches"},
		{"every credential", upstreamNamespaces, upstreamProcess, upstreamMappings, bindOf(T("/run/redoma-mcp")), upstreamAnnotation, "reaches"},
		{"redoma's sockets", upstreamNamespaces, upstreamProcess, upstreamMappings, bindOf(T("/run/redoma/xds")), upstreamAnnotation, "reaches"},
		{"Podman's", upstreamNamespaces, upstreamProcess, upstreamMappings, bindOf(T("/run/podman")), upstreamAnnotation, "reaches"},
		{"both annotations", upstreamNamespaces, upstreamProcess, upstreamMappings, "", map[string]string{"redoma/upstream": "grafana", "redoma/agent": "beta"}, "both agent"},
	} {
		expect(t, tc.name, createUpstream(t, tc.namespaces, tc.process, tc.linux, tc.mounts, tc.annotations), tc.want)
	}
}

// No agent mounts an upstream's credential, and none shares an id with an
// upstream's process.
func TestCreateRuntimeAgentAgainstUpstreams(t *testing.T) {
	for _, tc := range []struct {
		name, process, mounts, want string
	}{
		{"a credential directory", agentProcess, bindOf(T("/run/redoma-mcp/grafana")), "reaches"},
		{"a uid in the containers range", `{"user":{"uid":100500,"gid":1000},"noNewPrivileges":true,"capabilities":` + noCaps + `}`, "", "subordinate range"},
		{"a gid in it", `{"user":{"uid":1000,"gid":100500},"noNewPrivileges":true,"capabilities":` + noCaps + `}`, "", "subordinate range"},
		{"an additional gid in it", `{"user":{"uid":1000,"gid":1000,"additionalGids":[165535]},"noNewPrivileges":true,"capabilities":` + noCaps + `}`, "", "subordinate range"},
		{"just past it", `{"user":{"uid":165536,"gid":1000},"noNewPrivileges":true,"capabilities":` + noCaps + `}`, "", ""},
	} {
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		m := &fakeMounter{mounted: map[string]int{}}
		h := upstreamHook(t, cp, m)
		st := state(t, true)
		cfg := `{"ociVersion":"1.2.0","root":{"path":"rootfs"},"process":` + tc.process
		if tc.mounts != "" {
			cfg += `,"mounts":` + tc.mounts
		}
		st.Bundle = writeBundle(t, cfg+`,"linux":{"namespaces":`+fresh+`}}`)
		expect(t, tc.name, h.CreateRuntime(t.Context(), st), tc.want)
	}
}

func upstreamState() State {
	return State{ID: cid, Status: "running", Pid: 4242, Annotations: map[string]string{"redoma/upstream": "grafana"}}
}

func TestPoststartRegistersAnUpstreamWithLoAlone(t *testing.T) {
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	m := &fakeMounter{mounted: map[string]int{}}
	h := upstreamHook(t, cp, m)
	path := register.UpstreamNetnsPath("/run/netns", cid)
	m.mounted[path] = 4242
	var asked string
	h.Interfaces = func(netns string) ([]string, error) { asked = netns; return []string{"lo"}, nil }
	if err := h.Poststart(t.Context(), upstreamState()); err != nil {
		t.Fatal(err)
	}
	want := register.Request{Op: register.OpRegister, Container: cid, Upstream: "grafana", Netns: path}
	if asked != path || len(cp.requests) != 1 || cp.requests[0] != want {
		t.Errorf("listed %q, requests %+v; want %+v", asked, cp.requests, want)
	}
	if !m.has(path) {
		t.Error("the namespace was unmounted")
	}
}

func TestPoststartRefusesANetwork(t *testing.T) {
	for name, list := range map[string]func(string) ([]string, error){
		"a bridge":        func(string) ([]string, error) { return []string{"lo", "eth0"}, nil },
		"no lo":           func(string) ([]string, error) { return []string{"eth0"}, nil },
		"nothing":         func(string) ([]string, error) { return nil, nil },
		"cannot be read":  func(string) ([]string, error) { return nil, errors.New("setns: operation not permitted") },
		"lo, renamed one": func(string) ([]string, error) { return []string{"lo", "lo"}, nil },
	} {
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		m := &fakeMounter{mounted: map[string]int{}}
		h := upstreamHook(t, cp, m)
		path := register.UpstreamNetnsPath("/run/netns", cid)
		m.mounted[path] = 4242
		h.Interfaces = list
		err := h.Poststart(t.Context(), upstreamState())
		if err == nil || !strings.Contains(err.Error(), "not registered") {
			t.Errorf("%s: %v, want not registered", name, err)
		}
		if len(cp.requests) != 0 || m.has(path) {
			t.Errorf("%s: requests %v, mounted %v: want none and unmounted", name, cp.requests, m.mounted)
		}
	}
}

// A refused register unmounts too; an agent's container is not poststart's.
func TestPoststartOthers(t *testing.T) {
	cp := &fakeCP{answer: func(register.Request) string { return "refused" }}
	m := &fakeMounter{mounted: map[string]int{}}
	h := upstreamHook(t, cp, m)
	path := register.UpstreamNetnsPath("/run/netns", cid)
	m.mounted[path] = 4242
	h.Interfaces = func(string) ([]string, error) { return []string{"lo"}, nil }
	if err := h.Poststart(t.Context(), upstreamState()); err == nil || m.has(path) {
		t.Errorf("a refused register: %v, mounted %v", err, m.mounted)
	}
	cp.requests = nil
	st := state(t, true)
	h.Interfaces = func(string) ([]string, error) { t.Error("an agent's namespace listed"); return nil, nil }
	if err := h.Poststart(t.Context(), st); err != nil || len(cp.requests) != 0 {
		t.Errorf("an agent: %v, requests %v", err, cp.requests)
	}
}

func TestPoststopUpstream(t *testing.T) {
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	m := &fakeMounter{mounted: map[string]int{}}
	h := upstreamHook(t, cp, m)
	path := register.UpstreamNetnsPath("/run/netns", cid)
	m.mounted[path] = 4242
	if err := h.Poststop(t.Context(), upstreamState()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cp.ops(), []string{register.OpUnregister}) || m.has(path) {
		t.Errorf("ops %v, mounted %v", cp.ops(), m.mounted)
	}
}

func TestReadSubordinate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "subuid")
	if err := os.WriteFile(p, []byte("containers:100000:65536\nother:1:2\n# not ours\ncontainers:200000:10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readSubordinate(p, "containers")
	if err != nil || len(got) != 2 || got[0] != (idRange{100000, 65536}) || got[1] != (idRange{200000, 10}) {
		t.Errorf("readSubordinate = %v, %v", got, err)
	}
	if err := os.WriteFile(p, []byte("containers:x:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSubordinate(p, "containers"); err == nil {
		t.Error("a bad line: no error")
	}
	if got, err := readSubordinate(filepath.Join(dir, "none"), "containers"); got != nil || err != nil {
		t.Errorf("a missing file: %v, %v", got, err)
	}
}

// exempt lifts a forbidden path above the allowed directory, for what is in
// that directory, and nothing else.
func TestExempt(t *testing.T) {
	pl := policy{allowed: "/run/redoma-mcp/grafana"}
	for _, tc := range []struct {
		p, f string
		want bool
	}{
		{"/run/redoma-mcp/grafana", "/run/redoma-mcp", true},
		{"/run/redoma-mcp/grafana/token", "/run/redoma-mcp", true},
		{"/run/redoma-mcp", "/run/redoma-mcp", false},
		{"/run/redoma-mcp/grafanax", "/run/redoma-mcp", false},
		{"/run/redoma-mcp/other", "/run/redoma-mcp", false},
		// A forbidden path inside the allowed one stays forbidden.
		{"/run/redoma-mcp/grafana", "/run/redoma-mcp/grafana/inner", false},
		{"/run/redoma-mcp/grafana", "/run/redoma-mcp/grafana", false},
	} {
		if got := pl.exempt(tc.p, tc.f); got != tc.want {
			t.Errorf("exempt(%q, %q) = %v, want %v", tc.p, tc.f, got, tc.want)
		}
	}
	if (policy{}).exempt("/run/redoma-mcp/grafana", "/run/redoma-mcp") {
		t.Error("an agent's policy exempts something")
	}
}
