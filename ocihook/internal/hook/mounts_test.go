package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/common/register"
)

// tree is a host's forbidden paths, made in a temporary directory so that
// every path the tests bind exists (and resolves) wherever they run.
// testForbidden is the forbidden list over it, with the host's real /proc
// and /dev, which exist everywhere; testGraphroot is its Podman storage.
var (
	tree          string
	testForbidden []string
	testGraphroot string
)

// The layers of the tree's containers.
const (
	ownLayer       = "1111111111111111111111111111111111111111111111111111111111111111"
	realLayer      = "6176b7e5647558a594044462b2696daf92cf863ccc6f2eab7c4652d0a11f2c7c"
	volatileLayer  = "2222222222222222222222222222222222222222222222222222222222222222"
	transientLayer = "3333333333333333333333333333333333333333333333333333333333333333"
)

// The ids of a --rm container, whose record is volatile, and of one in a
// transient store.
var (
	volatileID  = strings.Repeat("b", 64)
	transientID = strings.Repeat("c", 64)
)

// T is p in the tree.
func T(p string) string { return filepath.Join(tree, p) }

func TestMain(m *testing.M) {
	if os.Getenv("REDOMA_HOOK_CHILD") != "" {
		// A process for TestBindProcess to hold: it runs until stdin closes.
		_, _ = os.Stdin.Read(make([]byte, 1))
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "hook-tree-")
	if err != nil {
		panic(err)
	}
	tree = dir
	for _, f := range ForbiddenSources {
		if f == "/proc" || f == "/dev" {
			testForbidden = append(testForbidden, f)
			continue
		}
		testForbidden = append(testForbidden, T(f))
	}
	testGraphroot = T(Graphroot)
	other := strings.Repeat("e", 64)
	for _, d := range []string{
		"/run/redoma-relay", "/run/redoma-relay-lead", "/run/redoma-spire/workload", "/run/redoma/names",
		"/etc/redoma", "/run/podman", "/var/lib/containers/storage/volumes/v/_data",
		"/var/lib/containers/storage/overlay-containers/" + cid + "/userdata/secrets",
		"/var/lib/containers/storage/overlay-containers/" + other + "/userdata",
		"/run/containers/storage/overlay-containers/" + cid + "/userdata",
		"/srv/redoma/work/a", "/run/redoma-xds",
	} {
		if err := os.MkdirAll(T(d), 0o755); err != nil {
			panic(err)
		}
	}
	for _, f := range []string{
		"/run/redoma-xds/xds.sock", "/run/redoma-relay/relay.sock", "/run/podman/podman.sock", "/run/redoma/names/a",
		"/run/redoma-spire/workload/agent.sock",
		"/var/lib/containers/storage/overlay-containers/" + cid + "/userdata/shm",
		"/var/lib/containers/storage/overlay-containers/" + cid + "/userdata/passwd",
		"/var/lib/containers/storage/overlay-containers/" + cid + "/userdata/config.json",
		"/var/lib/containers/storage/overlay-containers/" + other + "/userdata/shm",
		"/run/containers/storage/overlay-containers/" + cid + "/userdata/hosts",
	} {
		if err := os.WriteFile(T(f), nil, 0o600); err != nil {
			panic(err)
		}
	}
	// The storage's records of which layer is whose rootfs: the graphroot's,
	// a --rm container's apart (volatile), and a transient store's under the
	// runroot; and the rootfs of each.
	for file, recs := range map[string][]map[string]string{
		Graphroot + "/overlay-containers/containers.json": {
			{"id": cid, "layer": ownLayer},
			{"id": realID, "layer": realLayer},
			{"id": other, "layer": strings.Repeat("a", 64)},
		},
		Graphroot + "/overlay-containers/volatile-containers.json": {{"id": volatileID, "layer": volatileLayer}},
		Runroot + "/overlay-containers/containers.json":            {{"id": transientID, "layer": transientLayer}},
	} {
		records, _ := json.Marshal(recs)
		if err := os.WriteFile(T(file), records, 0o600); err != nil {
			panic(err)
		}
		for _, r := range recs {
			if err := os.MkdirAll(T(Graphroot+"/overlay/"+r["layer"]+"/merged"), 0o755); err != nil {
				panic(err)
			}
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestCreateRuntimeChecksTheMounts: no bind of the control plane's,
// SPIRE's, the launcher's, Podman's or the kernel's paths, of anything
// inside them, or of a directory above them; as written and as resolved
// through a symlink; no bind that does not resolve; no relative bind; no
// mount of another type than the runtime's own. The container's own
// userdata files, and the launcher's mounts (the user's directories), pass.
func TestCreateRuntimeChecksTheMounts(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "innocent")
	if err := os.Symlink(T("/run/redoma-xds"), link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(T("/run/redoma-xds/not-yet"), dangling); err != nil {
		t.Fatal(err)
	}
	bind := func(src string) string {
		return `[{"destination":"/proc","type":"proc","source":"proc"},{"destination":"/x","type":"bind","source":"` + src + `","options":["rbind","ro"]}]`
	}
	other := strings.Repeat("e", 64)
	userdata := T("/var/lib/containers/storage/overlay-containers") + "/"
	for name, tc := range map[string]struct {
		mounts string
		want   string
	}{
		"the launcher's":             {`[{"destination":"/work","type":"bind","source":"` + T("/srv/redoma/work/a") + `","options":["rbind","rw"]},{"destination":"/tmp","type":"tmpfs","source":"tmpfs"}]`, ""},
		"none":                       {``, ""},
		"its own shm":                {bind(userdata + cid + "/userdata/shm"), ""},
		"its own passwd":             {bind(userdata + cid + "/userdata/passwd"), ""},
		"Podman's runroot files":     {bind(T("/run/containers/storage/overlay-containers/" + cid + "/userdata/hosts")), ""},
		"another container's shm":    {bind(userdata + other + "/userdata/shm"), "reaches " + T("/var/lib/containers")},
		"its own config.json":        {bind(userdata + cid + "/userdata/config.json"), "reaches " + T("/var/lib/containers")},
		"its own userdata":           {bind(userdata + cid + "/userdata"), "reaches " + T("/var/lib/containers")},
		"a --secret":                 {bind(userdata + cid + "/userdata/secrets"), "reaches " + T("/var/lib/containers")},
		"a named volume":             {bind(T("/var/lib/containers/storage/volumes/v/_data")), "reaches " + T("/var/lib/containers")},
		"the xds socket's directory": {bind(T("/run/redoma-xds")), "reaches " + T("/run/redoma-xds")},
		"the xds socket":             {bind(T("/run/redoma-xds/xds.sock")), "reaches " + T("/run/redoma-xds")},
		"not normalized":             {bind(T("/run/./redoma-relay-lead/../redoma-relay-lead/")), "reaches " + T("/run/redoma-relay-lead")},
		"the relay":                  {bind(T("/run/redoma-relay/relay.sock")), "reaches " + T("/run/redoma-relay")},
		"SPIRE's workload API":       {bind(T("/run/redoma-spire/workload")), "reaches " + T("/run/redoma-spire")},
		// SPIRE makes the socket 0777: once the file itself is mounted, no
		// permission of its directory applies, and this check is all there is.
		"the Workload API socket itself":  {bind(T("/run/redoma-spire/workload/agent.sock")), "reaches " + T("/run/redoma-spire")},
		"the launcher's state":            {bind(T("/run/redoma/names/a")), "reaches " + T("/run/redoma")},
		"/etc/redoma":                     {bind(T("/etc/redoma")), "reaches " + T("/etc/redoma")},
		"Podman's socket":                 {bind(T("/run/podman/podman.sock")), "reaches " + T("/run/podman")},
		"a parent: /run":                  {bind(T("/run")), "reaches"},
		"a parent: /var/lib":              {bind(T("/var/lib")), "reaches " + T("/var/lib/containers")},
		"the root":                        {bind("/"), "reaches"},
		"a symlink to the xds socket":     {bind(link), "reaches " + T("/run/redoma-xds")},
		"a symlink that does not resolve": {bind(dangling), "does not resolve"},
		"a source that does not exist":    {bind(T("/srv/gone")), "does not resolve"},
		"another process's root":          {bind("/proc/1/root/run/redoma-xds"), "/proc"},
		"a descriptor":                    {bind("/dev/fd/3"), "/dev"},
		"a device":                        {bind("/dev/null"), "reaches /dev"},
		"a relative source":               {bind("../../../../run/redoma-xds"), "not an absolute path"},
		"a relative innocent source":      {bind("rootfs-extra"), "not an absolute path"},
		"bind by options only":            {`[{"destination":"/x","source":"` + T("/run/redoma-xds") + `","options":["bind"]}]`, "reaches " + T("/run/redoma-xds")},
		"another file system":             {`[{"destination":"/x","type":"ext4","source":"/dev/sda1"}]`, `type "ext4"`},
		"an overlay":                      {`[{"destination":"/x","type":"overlay","source":"overlay","options":["lowerdir=/run/redoma-xds"]}]`, `type "overlay"`},
	} {
		expect(t, name, create(t, agentProcess, tc.mounts), tc.want)
	}
}

func TestReach(t *testing.T) {
	pl := newPolicy(ForbiddenSources, Graphroot, cid, nil, "")
	for p, want := range map[string]string{
		"/run/redoma":          "/run/redoma",
		"/run/redoma/active":   "/run/redoma",
		"/run/redoma-relayx":   "",
		"/srv":                 "",
		"/etc":                 "/etc/redoma",
		"/var/lib/containersx": "",
		"/proc/self/fd/3":      "/proc",
		"/var/lib/containers/storage/vfs-containers/" + cid + "/userdata/group":           "",
		"/var/lib/containers/storage/overlay-containers/" + cid + "/userdata/secrets/key": "/var/lib/containers",
		"/var/lib/containers/storage/overlay-containers/" + cid + "x/userdata/shm":        "/var/lib/containers",
	} {
		if got := pl.reach(p); got != want {
			t.Errorf("reach(%q) = %q, want %q", p, got, want)
		}
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"/a/b", "/", true},
		{"/", "/a", true},
		{"/a", "/a", true},
		{"/a/b", "/a", true},
		{"/a", "/a/b", true},
		{"/ab", "/a", false},
		{"/a", "/ab", false},
	} {
		if got := related(tc.a, tc.b); got != tc.want {
			t.Errorf("related(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// TestCreateRuntimeChecksTheDevices: the runtime's default devices only,
// in linux.devices and in the cgroup's device rules; null is no devices.
func TestCreateRuntimeChecksTheDevices(t *testing.T) {
	for name, tc := range map[string]struct {
		linux string
		want  string
	}{
		"none":                 {`"devices":[]`, ""},
		"null":                 {`"devices":null`, ""},
		"the defaults":         {`"devices":[{"path":"/dev/null","type":"c","major":1,"minor":3},{"path":"/dev/urandom","type":"c","major":1,"minor":9},{"path":"/dev/ptmx","type":"c","major":5,"minor":2}]`, ""},
		"a disk":               {`"devices":[{"path":"/dev/sda","type":"b","major":8,"minor":0}]`, "/dev/sda (b 8:0)"},
		"a disk named as null": {`"devices":[{"path":"/dev/null","type":"b","major":8,"minor":0}]`, "/dev/null (b 8:0)"},
		"kmsg":                 {`"devices":[{"path":"/dev/kmsg","type":"c","major":1,"minor":11}]`, "/dev/kmsg"},
		"fuse":                 {`"devices":[{"path":"/dev/fuse","type":"c","major":10,"minor":229}]`, "/dev/fuse"},
		"no device rules":      {`"resources":null`, ""},
		"deny all":             {`"resources":{"devices":[{"allow":false,"access":"rwm"}]}`, ""},
		"allow the defaults":   {`"resources":{"devices":[{"allow":false,"access":"rwm"},{"allow":true,"type":"c","major":1,"minor":3,"access":"rwm"},{"allow":true,"type":"c","major":136,"access":"rwm"}]}`, ""},
		"allow all":            {`"resources":{"devices":[{"allow":true,"access":"rwm"}]}`, "allows the devices a *:*"},
		// The reviewer's probe: read as a deny by the hook, as an allow of
		// the host's disk by runc's encoding/json.
		"Allow in another case":  {`"resources":{"devices":[{"Allow":true,"type":"b","major":8,"minor":0,"access":"rwm"}]}`, "in another case"},
		"Access in another case": {`"resources":{"devices":[{"allow":false,"Access":"rwm"}]}`, "in another case"},
		"allow every block":      {`"resources":{"devices":[{"allow":true,"type":"b","access":"rwm"}]}`, "allows the devices b *:*"},
		"allow a disk":           {`"resources":{"devices":[{"allow":true,"type":"b","major":8,"minor":0,"access":"rw"}]}`, "allows the devices b 8:0"},
	} {
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		h, _ := newHook(cp, &fakeMounter{mounted: map[string]int{}})
		st := state(t, true)
		st.Bundle = writeBundle(t, `{"ociVersion":"1.2.0","root":{"path":"rootfs"},"process":`+agentProcess+`,"linux":{"namespaces":`+fresh+`,`+tc.linux+`}}`)
		expect(t, name, h.CreateRuntime(t.Context(), st), tc.want)
	}
}

// hostMounts is a host's mountinfo, in part.
const hostMounts = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
25 22 0:25 / /run rw,nosuid,nodev shared:5 - tmpfs tmpfs rw
26 22 0:5 / /dev rw,nosuid shared:2 - devtmpfs devtmpfs rw
27 22 0:22 / /proc rw,nosuid,nodev,noexec shared:12 - proc proc rw
40 25 0:40 / /run/user/1000 rw - tmpfs tmpfs rw
41 26 0:41 / /dev/shm rw - tmpfs tmpfs rw
`

// rootfs is the container's, as Podman 4.9 makes it.
const rootfs = "/var/lib/containers/storage/overlay/0f0f/merged"

// TestCheckMountinfo: what the container has mounted, against the host's
// forbidden places, by device and path within the device: a second name
// of a place is caught, and so is any part of a file system that is
// forbidden whole (the host's /dev); the container's own files, the
// runtime's masks of the spec's maskedPaths, and the copies of the host's
// mounts that a namespace not yet pivoted holds, are not.
func TestCheckMountinfo(t *testing.T) {
	host := parseMountinfo(hostMounts)
	pl := newPolicy(ForbiddenSources, Graphroot, cid, []string{"/proc/kcore", "/proc/keys"}, rootfs)
	base := hostMounts +
		"60 22 0:60 / " + rootfs + " rw - overlay overlay rw\n" +
		"61 60 0:61 / " + rootfs + "/proc rw - proc proc rw\n" +
		"62 60 0:62 / " + rootfs + "/dev rw - tmpfs tmpfs rw\n" +
		"63 60 0:63 / " + rootfs + "/dev/shm rw - tmpfs shm rw\n" +
		"64 60 8:1 /var/lib/containers/storage/overlay-containers/" + cid + "/userdata/passwd " + rootfs + "/etc/passwd rw - ext4 /dev/sda1 rw\n" +
		"65 60 0:25 /containers/storage/overlay-containers/" + cid + "/userdata/hosts " + rootfs + "/etc/hosts rw - tmpfs tmpfs rw\n" +
		"66 60 8:1 /srv/redoma/work/a " + rootfs + "/work rw - ext4 /dev/sda1 rw\n" +
		"67 61 0:5 /null " + rootfs + "/proc/kcore ro - devtmpfs udev rw\n"
	line := func(dev, root, at string) string {
		return "70 60 " + dev + " " + root + " " + rootfs + at + " rw - tmpfs tmpfs rw\n"
	}
	for name, tc := range map[string]struct {
		extra string
		want  string
	}{
		"Podman's own":                    {"", ""},
		"a mask of a maskedPath":          {line("0:5", "/null", "/proc/keys"), ""},
		"not /null at a maskedPath":       {line("0:5", "/sda", "/proc/keys"), "reaches /dev"},
		"all of /dev at a maskedPath":     {line("0:5", "/", "/proc/keys"), "reaches /dev"},
		"the host's null elsewhere":       {line("0:5", "/null", "/x"), "reaches /dev"},
		"the host's /dev/sda by any name": {line("0:5", "/sda", "/x"), "reaches /dev"},
		"the host's /dev":                 {line("0:5", "/", "/x"), "reaches /dev"},
		"the host's /dev/shm":             {line("0:41", "/", "/x"), "reaches /dev"},
		"the xds directory":               {line("0:25", "/redoma-xds", "/x"), "reaches /run/redoma-xds"},
		"the xds socket, any name":        {line("0:25", "/redoma-xds/xds.sock", "/x"), "reaches /run/redoma-xds"},
		"all of /run":                     {line("0:25", "/", "/x"), "reaches"},
		"another container's passwd":      {line("8:1", "/var/lib/containers/storage/overlay-containers/"+strings.Repeat("e", 64)+"/userdata/passwd", "/y"), "reaches /var/lib/containers"},
		"/etc/redoma":                     {line("8:1", "/etc/redoma", "/y"), "reaches /etc/redoma"},
		"a path with a space":             {line("8:1", `/etc/redoma\040x`, "/y"), ""},
		"another user's runtime dir":      {line("0:40", "/", "/y"), ""},
	} {
		expect(t, name, pl.checkMountinfo(host, parseMountinfo(base+tc.extra), rootfs), tc.want)
	}
	pivoted := parseMountinfo("80 79 0:60 / / rw - overlay overlay rw\n81 80 0:25 /redoma-relay-lead /x rw - tmpfs tmpfs rw\n")
	expect(t, "pivoted", pl.checkMountinfo(host, pivoted, rootfs), "reaches /run/redoma-relay-lead")
	expect(t, "before any mount", pl.checkMountinfo(host, parseMountinfo(hostMounts), rootfs), "")
}

// podmanSpec is the real capture's spec (podman 4.9.3, crun, an agent the
// launcher started, env left out), with what a hook agent's has instead:
// its own network namespace, root.path, and Podman's default maskedPaths
// (pkg/specgen/generate BlockAccessToKernelFilesystems), which the capture
// left out.
func podmanSpec(t *testing.T, sources func(string) string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/podman-4.9.3/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	linux := spec["linux"].(map[string]any)
	for _, ns := range linux["namespaces"].([]any) {
		delete(ns.(map[string]any), "path")
	}
	linux["maskedPaths"] = []any{
		"/proc/acpi", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/timer_list",
		"/proc/timer_stats", "/proc/sched_debug", "/proc/scsi", "/sys/firmware", "/sys/fs/selinux", "/sys/dev/block",
	}
	spec["root"] = map[string]any{"path": "/var/lib/containers/storage/overlay/6176b7e5647558a594044462b2696daf92cf863ccc6f2eab7c4652d0a11f2c7c/merged"}
	for _, m := range spec["mounts"].([]any) {
		m := m.(map[string]any)
		if src, _ := m["source"].(string); filepath.IsAbs(src) {
			m["source"] = sources(src)
		}
	}
	return spec
}

// realID is the captured container's id.
const realID = "1eafece6792ae4cd09d2987aeae4d20e718947ac29fc9e27b6174d16b283ee3a"

// TestRealPodmanSpec: the captured agent passes every check of the spec,
// but for its /etc/agent-name from /run/redoma/names, a mount of the
// launcher's state; its sources are recreated in the tree, so that they
// resolve.
func TestRealPodmanSpec(t *testing.T) {
	spec := podmanSpec(t, func(src string) string {
		p := T(src)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); err != nil {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return p
	})
	root := spec["root"].(map[string]any)
	root["path"] = T(Graphroot + "/overlay/" + realLayer + "/merged")
	run := func(spec map[string]any) error {
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		h, _ := newHook(cp, &fakeMounter{mounted: map[string]int{}})
		st := state(t, true)
		st.ID, st.Bundle = realID, dir
		return h.CreateRuntime(t.Context(), st)
	}
	expect(t, "as captured", run(spec), "reaches "+T("/run/redoma"))
	var kept []any
	for _, m := range spec["mounts"].([]any) {
		if m.(map[string]any)["destination"] != "/etc/agent-name" {
			kept = append(kept, m)
		}
	}
	spec["mounts"] = kept
	expect(t, "without /etc/agent-name", run(spec), "")

	// Its root as the storage records it is its own; another container's
	// merged dir, or the host's /dev/shm, is not, by any name: the root is
	// taken resolved, and one that does not resolve is refused.
	links := t.TempDir()
	for name, target := range map[string]string{
		"own":      T(Graphroot + "/overlay/" + realLayer + "/merged"),
		"other":    T(Graphroot + "/overlay/" + strings.Repeat("a", 64) + "/merged"),
		"dangling": T(Graphroot + "/overlay/" + strings.Repeat("d", 64) + "/merged"),
	} {
		if err := os.Symlink(target, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct{ path, want string }{
		"its own overlay":                   {T(Graphroot + "/overlay/" + realLayer + "/merged"), ""},
		"its own overlay through a symlink": {filepath.Join(links, "own"), ""},
		"its own overlay, uncleaned":        {T(Graphroot + "/overlay/./" + realLayer + "/merged/"), ""},
		"another container's overlay":       {T(Graphroot + "/overlay/" + strings.Repeat("a", 64) + "/merged"), "is not its own"},
		"a symlink to another's overlay":    {filepath.Join(links, "other"), "is not its own"},
		"a symlink that does not resolve":   {filepath.Join(links, "dangling"), "does not resolve"},
		"a root that does not exist":        {T(Graphroot + "/overlay/" + strings.Repeat("d", 64) + "/merged"), "does not resolve"},
		"the host's /dev/shm":               {"/dev/shm", "is not its own"},
		"the host's /dev/pts":               {"/dev/pts", "is not its own"},
	} {
		root["path"] = tc.path
		expect(t, "root: "+name, run(spec), tc.want)
	}
}

// TestContainerRootfs: the rootfs is the layer of the record with the id,
// exactly, in any of the four record files: the graphroot's and the
// runroot's (transient_store), containers.json and volatile-containers.json
// (a --rm container's); none for an id in none, a prefix of one, or a layer
// that is not a layer id.
func TestContainerRootfs(t *testing.T) {
	graph, run := t.TempDir(), t.TempDir()
	layer := func(c string) string { return strings.Repeat(c, 64) }
	for path, recs := range map[string]string{
		graph + "/overlay-containers/containers.json":          `[{"id":"a","layer":"` + layer("1") + `"},{"id":"bad","layer":"../../etc"}]`,
		graph + "/overlay-containers/volatile-containers.json": `[{"id":"b","layer":"` + layer("2") + `"}]`,
		run + "/overlay-containers/containers.json":            `[{"id":"c","layer":"` + layer("3") + `"}]`,
		run + "/overlay-containers/volatile-containers.json":   `[{"id":"d","layer":"` + layer("4") + `"}]`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(recs), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{
		"a": layer("1"), "b": layer("2"), "c": layer("3"), "d": layer("4"),
		"e": "", "": "", "bad": "", "ab": "",
	} {
		got := containerRootfs(graph, run, id)
		if want != "" {
			want = filepath.Join(graph, "overlay", want, "merged")
		}
		if got != want {
			t.Errorf("%q: %q, want %q", id, got, want)
		}
	}
	// A record file that is not JSON gives none, and does not hide the
	// others.
	if err := os.WriteFile(graph+"/overlay-containers/containers.json", []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := containerRootfs(graph, run, "a"); got != "" {
		t.Errorf("a, broken record: %q, want none", got)
	}
	if got, want := containerRootfs(graph, run, "d"), filepath.Join(graph, "overlay", layer("4"), "merged"); got != want {
		t.Errorf("d, beside a broken record: %q, want %q", got, want)
	}
}

// TestCreateRuntimeOwnRootfs: a container's root is its own wherever its
// storage records it: a --rm container's volatile record and a transient
// store's under the runroot both count; a container with no record has no
// rootfs of its own, and a root in the storage is refused.
func TestCreateRuntimeOwnRootfs(t *testing.T) {
	merged := func(layer string) string { return T(Graphroot + "/overlay/" + layer + "/merged") }
	for name, tc := range map[string]struct{ id, root, want string }{
		"containers.json":              {cid, merged(ownLayer), ""},
		"volatile-containers.json":     {volatileID, merged(volatileLayer), ""},
		"transient, under the runroot": {transientID, merged(transientLayer), ""},
		"in no record":                 {strings.Repeat("f", 64), merged(ownLayer), "is not its own"},
		"another's volatile rootfs":    {cid, merged(volatileLayer), "is not its own"},
		"another's transient rootfs":   {volatileID, merged(transientLayer), "is not its own"},
	} {
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		h, _ := newHook(cp, &fakeMounter{mounted: map[string]int{}})
		st := state(t, true)
		st.ID = tc.id
		st.Bundle = writeBundle(t, `{"ociVersion":"1.2.0","root":{"path":"`+tc.root+`"},"process":`+agentProcess+`,"linux":{"namespaces":`+fresh+`}}`)
		expect(t, name, h.CreateRuntime(t.Context(), st), tc.want)
	}
}

// TestRealPodmanMountinfo: the captured container's mountinfo (after its
// start: pivoted), against a host's built from the same devices, passes
// but for /etc/agent-name: crun's masks (the host's devtmpfs /null on
// /proc/kcore and the rest), its empty tmpfs masks, the container's own
// cgroup, /dev/console from its own devpts, and its shm, a tmpfs of its
// own on the host under /var/lib/containers.
func TestRealPodmanMountinfo(t *testing.T) {
	read := func(name string) []mountEntry {
		raw, err := os.ReadFile(filepath.Join("testdata/podman-4.9.3", name))
		if err != nil {
			t.Fatal(err)
		}
		return parseMountinfo(string(raw))
	}
	host, ctr := read("host-mountinfo.txt"), read("mountinfo.txt")
	spec := podmanSpec(t, func(s string) string { return s })
	var masked []string
	for _, p := range spec["linux"].(map[string]any)["maskedPaths"].([]any) {
		masked = append(masked, p.(string))
	}
	rootfs := spec["root"].(map[string]any)["path"].(string)
	pl := newPolicy(ForbiddenSources, Graphroot, realID, masked, rootfs)
	expect(t, "as captured", pl.checkMountinfo(host, ctr, rootfs), "/redoma/names/beta mounted at /etc/agent-name")
	var kept []mountEntry
	for _, c := range ctr {
		if c.point != "/etc/agent-name" {
			kept = append(kept, c)
		}
	}
	expect(t, "without /etc/agent-name", pl.checkMountinfo(host, kept, rootfs), "")
	// Without the masks in the spec, the host's /null on /proc/kcore is
	// the host's /dev, and refused: the exemption is the spec's own.
	expect(t, "masks not in the spec", newPolicy(ForbiddenSources, Graphroot, realID, nil, rootfs).checkMountinfo(host, kept, rootfs), "reaches /dev")
	// Another container's shm, a tmpfs of its own on the host inside
	// /var/lib/containers, is refused by its device.
	other := append(kept, mountEntry{dev: "0:70", root: "/", point: "/x"})
	expect(t, "another container's shm", pl.checkMountinfo(host, other, rootfs), "reaches /var/lib/containers")
	// A root that is another container's overlay (podman run --rootfs
	// <its merged dir>), or the host's /dev/shm or /dev/pts, is refused:
	// only the overlay the storage gives this container is its own.
	host = append(host,
		mountEntry{dev: "0:81", root: "/", point: "/var/lib/containers/storage/overlay/" + strings.Repeat("a", 64) + "/merged"})
	for name, dev := range map[string]string{"another container's overlay": "0:81", "the host's /dev/shm": "0:27", "the host's /dev/pts": "0:24"} {
		probe := append([]mountEntry{{dev: dev, root: "/", point: "/"}}, kept[1:]...)
		expect(t, "/ is "+name, pl.checkMountinfo(host, probe, rootfs), "reaches")
	}
	// Its own overlay is not a forbidden place only because the storage
	// says it is this container's: with none known, it is.
	expect(t, "no storage record", newPolicy(ForbiddenSources, Graphroot, realID, masked, "").checkMountinfo(host, kept, rootfs), "reaches /var/lib/containers")
}

// TestCreateRuntimeReadsMountinfo: createRuntime refuses on what the
// container's mountinfo shows, and when it cannot be read.
func TestCreateRuntimeReadsMountinfo(t *testing.T) {
	for name, tc := range map[string]struct {
		ctr  string
		fail bool
		want string
	}{
		"clean":      {"", false, ""},
		"a bad bind": {"70 60 0:25 /redoma-xds ROOT/x rw - tmpfs tmpfs rw\n", false, "reaches /run/redoma-xds"},
		"unreadable": {"", true, "the container's mounts"},
	} {
		cp := &fakeCP{answer: func(register.Request) string { return "" }}
		m := &fakeMounter{mounted: map[string]int{}}
		h, _ := newHook(cp, m)
		h.Forbidden, h.Graphroot = nil, ""
		st := state(t, true)
		st.Bundle = writeBundle(t, `{"ociVersion":"1.2.0","root":{"path":"rootfs"},"process":`+agentProcess+`,"linux":{"namespaces":`+fresh+`}}`)
		root, err := filepath.EvalSymlinks(filepath.Join(st.Bundle, "rootfs"))
		if err != nil {
			t.Fatal(err)
		}
		// The container's mounts are under its rootfs, as resolved.
		ctr := strings.ReplaceAll(tc.ctr, "ROOT", root)
		h.Mountinfo = func(pid string) ([]mountEntry, error) {
			if pid == "self" {
				return parseMountinfo(hostMounts), nil
			}
			if tc.fail {
				return nil, os.ErrNotExist
			}
			return parseMountinfo(ctr), nil
		}
		expect(t, name, h.CreateRuntime(t.Context(), st), tc.want)
		if tc.want != "" && (len(m.mounted) != 0 || len(cp.requests) != 0) {
			t.Errorf("%s: refused, but mounts %v, requests %v", name, m.mounted, cp.requests)
		}
	}
}
