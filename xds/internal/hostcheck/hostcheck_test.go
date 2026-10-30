package hostcheck

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSocketsForbidden: a socket counts where its directory resolves.
func TestSocketsForbidden(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "spire", "workload")
	outside := filepath.Join(dir, "elsewhere")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "spire", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	sources := []string{filepath.Join(dir, "spire")}
	for socket, want := range map[string]string{
		filepath.Join(inside, "agent.sock"):  "",
		filepath.Join(outside, "agent.sock"): "does not keep",
		// The path is under the forbidden one, but it resolves outside it.
		filepath.Join(link, "agent.sock"):                 "does not keep",
		filepath.Join(dir, "missing", "dir", "a.sock"):    "its directory",
		filepath.Join(inside, "..", "workload", "x.sock"): "",
	} {
		err := SocketsForbidden([]string{socket}, sources)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("SocketsForbidden(%s) = %v, want %q", socket, err, want)
		}
	}
}

// TestWorkloadDir: no permission for others, no group write, and a group
// whose members are the allowed services (and root) only.
func TestWorkloadDir(t *testing.T) {
	gid := os.Getgid()
	me := os.Getuid()
	g := strconv.Itoa(gid)
	for name, tc := range map[string]struct {
		mode          os.FileMode
		passwd, group string
		allowed       []uint32
		want          string
	}{
		"the services only": {
			0o750, "root:x:0:0::/root:/bin/sh\nredoma-sds:x:961:961::/:/usr/sbin/nologin\nredoma-relay:x:963:963::/:/usr/sbin/nologin\n",
			"redoma-spire:x:" + g + ":redoma-sds,redoma-relay\n",
			[]uint32{961, 963},
			"",
		},
		"its primary group": {0o750, "me:x:" + strconv.Itoa(me) + ":" + g + "::/:/bin/sh\n", "", []uint32{961, 963, uint32(me)}, ""},
		"open to others":    {0o755, "", "", []uint32{961}, "grants others"},
		"group-writable":    {0o770, "", "", []uint32{961}, "group may write"},
		"a user listed":     {0o750, "redoma-sds:x:961:961::/:/bin/false\nagent:x:1000:1000::/:/bin/sh\n", "redoma-spire:x:" + g + ":redoma-sds,agent\n", []uint32{961, 963}, "agent (uid 1000)"},
		"a primary member":  {0o750, "agent:x:1000:" + g + "::/:/bin/sh\n", "", []uint32{961, 963}, "agent (uid 1000)"},
		"an unknown name":   {0o750, "", "redoma-spire:x:" + g + ":ghost\n", []uint32{961}, "ghost"},
	} {
		dir := filepath.Join(t.TempDir(), "workload")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, tc.mode); err != nil {
			t.Fatal(err)
		}
		etc := t.TempDir()
		acc := Accounts{Passwd: filepath.Join(etc, "passwd"), Group: filepath.Join(etc, "group")}
		for path, content := range map[string]string{acc.Passwd: tc.passwd, acc.Group: tc.group} {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		err := WorkloadDir(dir, tc.allowed, acc)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: WorkloadDir = %v, want %q", name, err, tc.want)
		}
	}
}
