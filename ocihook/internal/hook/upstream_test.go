package hook

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	mustChmod(t, dir, 0o755) // the files' directory is trusted as theirs
	h.SubUID, h.SubGID = filepath.Join(dir, "subuid"), filepath.Join(dir, "subgid")
	if err := os.WriteFile(h.SubUID, []byte("someuser:300000:65536\ncontainers:100000:65536\ncontainers:200000:1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.SubGID, []byte("containers:100000:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.TokenRoot = T("/run/redoma-mcp")
	if err := os.MkdirAll(filepath.Join(h.TokenRoot, "grafana"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.TokenRoot, "grafana", "token"), []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Symlinks across the credential directory's edge, both ways.
	for link, target := range map[string]string{
		filepath.Join(h.TokenRoot, "grafana", "out"): T("/srv/redoma/work/a"),
		T("/srv/redoma/work/in"):                     filepath.Join(h.TokenRoot, "grafana"),
	} {
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"grafana", "other"} {
		if err := os.MkdirAll(filepath.Join(h.TokenRoot, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func bindOf(src string) string {
	return bindWith(src, `["rbind","ro"]`, "")
}

// bindWith is a bind of src with options (JSON) and extra mount fields.
func bindWith(src, options, extra string) string {
	m := `{"destination":"/run/secrets/x","type":"bind","source":"` + src + `","options":` + options
	if extra != "" {
		m += `,` + extra
	}
	return `[` + m + `}]`
}

// mountMappings are a mount's own uid and gid mappings.
const mountMappings = `"uidMappings":[{"containerID":0,"hostID":100000,"size":65536}],"gidMappings":[{"containerID":0,"hostID":100000,"size":65536}]`

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
	ok("a file in it", createUpstream(t, upstreamNamespaces, upstreamProcess, upstreamMappings, bindOf(T("/run/redoma-mcp/grafana/token")), upstreamAnnotation))
	ok("its own directory idmapped, read-only", createUpstream(t, upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana"), `["rbind","ro","idmap"]`, ""), upstreamAnnotation))
	ok("its own directory with mappings, read-only", createUpstream(t, upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana"), `["rbind","ro"]`, mountMappings), upstreamAnnotation))
	ok("an unmapped bind elsewhere", createUpstream(t, upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/srv/redoma/work/a"), `["rbind"]`, ""), upstreamAnnotation))
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
		// An idmapped bind lets the upstream's root own what host root owns
		// under it: only its own credential directory, read-only.
		{"an idmapped bind elsewhere", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/srv/redoma/work/a"), `["rbind","ro","idmap"]`, ""), upstreamAnnotation, "idmapped"},
		{"a ridmapped bind elsewhere", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/srv/redoma/work/a"), `["rbind","ridmap"]`, ""), upstreamAnnotation, "idmapped"},
		{"an idmap with its mappings elsewhere", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/srv/redoma/work/a"), `["rbind","idmap=uids=0-100000-65536;gids=0-100000-65536"]`, ""), upstreamAnnotation, "idmapped"},
		{"a bind elsewhere with mappings", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/srv/redoma/work/a"), `["rbind","ro"]`, mountMappings), upstreamAnnotation, "idmapped"},
		{"a bind elsewhere with uid mappings only", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/srv/redoma/work/a"), `["rbind","ro"]`, `"uidMappings":[{"containerID":0,"hostID":100000,"size":1}]`), upstreamAnnotation, "idmapped"},
		{"a bind elsewhere with gid mappings only", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/srv/redoma/work/a"), `["rbind","ro"]`, `"gidMappings":[{"containerID":0,"hostID":100000,"size":1}]`), upstreamAnnotation, "idmapped"},
		{"its own directory idmapped, writable", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana"), `["rbind","idmap"]`, ""), upstreamAnnotation, "writable"},
		{"its own directory, writable", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana"), `["rbind","rw"]`, ""), upstreamAnnotation, "writable"},
		{"a file in it, writable", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana/token"), `["rbind"]`, ""), upstreamAnnotation, "writable"},
		// Options apply in order, so rw after ro wins: any rw is refused.
		{"its own directory, ro then rw", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana"), `["rbind","ro","rw"]`, ""), upstreamAnnotation, "writable"},
		{"its own directory idmapped, ro then rw", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana"), `["rbind","ro","idmap","rw"]`, ""), upstreamAnnotation, "writable"},
		{"its own directory, rw then ro", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana"), `["rbind","rw","ro"]`, ""), upstreamAnnotation, "writable"},
		// A symlink that starts on one side of the credential directory and
		// resolves to the other is judged on both: idmapped, it is refused.
		{"an idmapped symlink in it to outside", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/run/redoma-mcp/grafana/out"), `["rbind","ro","idmap"]`, ""), upstreamAnnotation, "idmapped"},
		{"an idmapped symlink outside to it", upstreamNamespaces, upstreamProcess, upstreamMappings, bindWith(T("/srv/redoma/work/in"), `["rbind","ro","idmap"]`, ""), upstreamAnnotation, "idmapped"},
		// The name is a name, never a path: x/../other would exempt another
		// upstream's credential directory.
		{"a traversal name", upstreamNamespaces, upstreamProcess, upstreamMappings, bindOf(T("/run/redoma-mcp/other")), map[string]string{"redoma/upstream": "x/../other"}, "not an MCP upstream name"},
		{"a traversal name, no mount", upstreamNamespaces, upstreamProcess, upstreamMappings, "", map[string]string{"redoma/upstream": "../other"}, "not an MCP upstream name"},
		{"an absolute name", upstreamNamespaces, upstreamProcess, upstreamMappings, "", map[string]string{"redoma/upstream": "/run/redoma-mcp/other"}, "not an MCP upstream name"},
		{"an upper-case name", upstreamNamespaces, upstreamProcess, upstreamMappings, "", map[string]string{"redoma/upstream": "Grafana"}, "not an MCP upstream name"},
		{"an empty name", upstreamNamespaces, upstreamProcess, upstreamMappings, "", map[string]string{"redoma/upstream": ""}, "not an MCP upstream name"},
		{"the defaults' name", upstreamNamespaces, upstreamProcess, upstreamMappings, "", map[string]string{"redoma/upstream": "_default"}, "not an MCP upstream name"},
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

// Poststart registers no upstream whose name is not one, whatever the
// hook definitions' filter let through.
func TestPoststartRefusesABadName(t *testing.T) {
	for _, name := range []string{"x/../other", "../other", "Grafana", "", "_default"} {
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		m := &fakeMounter{mounted: map[string]int{}}
		h := upstreamHook(t, cp, m)
		h.Interfaces = func(string) ([]string, error) { return []string{"lo"}, nil }
		st := upstreamState()
		st.Annotations = map[string]string{"redoma/upstream": name}
		if err := h.Poststart(t.Context(), st); err == nil || !strings.Contains(err.Error(), "not an MCP upstream name") {
			t.Errorf("%q: %v", name, err)
		}
		if len(cp.requests) != 0 {
			t.Errorf("%q: registered %v", name, cp.requests)
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

// me is the test's uid, the owner its files have.
var me = uint32(os.Getuid())

func TestReadSubordinate(t *testing.T) {
	dir := t.TempDir()
	mustChmod(t, dir, 0o755)
	p := filepath.Join(dir, "subuid")
	// An entry by name and one by uid (shadow-utils takes either), another
	// user's, a comment.
	if err := os.WriteFile(p, []byte("containers:100000:65536\nother:1:2\n# not ours\n999:200000:10\n998:300000:10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readSubordinate(p, "containers", "999", me)
	if err != nil || len(got) != 2 || got[0] != (idRange{100000, 65536}) || got[1] != (idRange{200000, 10}) {
		t.Errorf("readSubordinate = %v, %v", got, err)
	}
	if got, err := readSubordinate(p, "containers", "", me); err != nil || len(got) != 1 {
		t.Errorf("no uid: %v, %v; want the named entry alone", got, err)
	}
	if err := os.WriteFile(p, []byte("containers:x:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSubordinate(p, "containers", "999", me); err == nil {
		t.Error("a bad line: no error")
	}
	if got, err := readSubordinate(filepath.Join(dir, "none"), "containers", "999", me); got != nil || err != nil {
		t.Errorf("a missing file: %v, %v", got, err)
	}
}

// The subordinate id files decide what an upstream may map: they are
// trusted as agents.txt is, or refused.
func TestReadSubordinateRefusesAnUntrustedFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir, p string)
		owner uint32
		want  string
	}{
		{"another owner", nil, me + 1, "must be owned by uid"},
		{"group-writable", func(t *testing.T, dir, p string) { mustChmod(t, p, 0o664) }, me, "writable by it alone"},
		{"world-writable", func(t *testing.T, dir, p string) { mustChmod(t, p, 0o646) }, me, "writable by it alone"},
		{"a writable directory", func(t *testing.T, dir, p string) { mustChmod(t, dir, 0o777) }, me, "writable by it alone"},
		{"a symlink", func(t *testing.T, dir, p string) {
			if err := os.Rename(p, p+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(p+".real", p); err != nil {
				t.Fatal(err)
			}
		}, me, "symbolic links"},
		{"a FIFO", func(t *testing.T, dir, p string) {
			os.Remove(p)
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Fatal(err)
			}
		}, me, "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "etc")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, "subuid")
			if err := os.WriteFile(p, []byte("containers:100000:65536\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, dir, p)
			}
			if _, err := readSubordinate(p, "containers", "", tc.owner); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v; want %q", err, tc.want)
			}
		})
	}
}

func mustChmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// containers ranges that hold root or a service's id are refused, for
// upstreams and agents alike: an upstream could map that id.
func TestSubordinateRangesExcludeRootAndServices(t *testing.T) {
	for _, tc := range []struct {
		subuid, subgid, want string
	}{
		{"containers:0:65536\n", "containers:100000:65536\n", "holds root's id 0"},
		{"containers:100000:65536\n", "containers:0:1\n", "holds root's id 0"},
		{"containers:900:100\n", "containers:100000:65536\n", "holds 961, redoma-sds's"},
		{"containers:100000:65536\n", "containers:964:1\n", "holds 964, redoma-lead's"},
		{"999:963:1\n", "containers:100000:65536\n", "holds 963, redoma-relay's"},
		{"containers:100000:65536\n", "containers:100000:65536\n", ""},
	} {
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		m := &fakeMounter{mounted: map[string]int{}}
		h := upstreamHook(t, cp, m)
		if err := os.WriteFile(h.SubUID, []byte(tc.subuid), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.SubGID, []byte(tc.subgid), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := h.subordinate()
		expect(t, tc.subuid+tc.subgid, err, tc.want)
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
