package hook

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sleeper starts a process that runs until the test ends: the hook's tests
// binary itself, with REDOMA_HOOK_CHILD set (TestMain).
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "REDOMA_HOOK_CHILD=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// fakeProcDir writes <proc>/<pid>/cgroup and links <proc>/<pid>/ns/net to
// netns, for processBinder.
func fakeProcDir(t *testing.T, pid int, netns, cgroup string) string {
	t.Helper()
	proc := t.TempDir()
	dir := filepath.Join(proc, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(netns, filepath.Join(dir, "ns", "net")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o644); err != nil {
		t.Fatal(err)
	}
	return proc
}

// TestBindProcess: the pidfd is handed out only for the process in the
// mounted namespace and the container's cgroup.
func TestBindProcess(t *testing.T) {
	pid := sleeper(t)
	scope := "0::/machine.slice/libpod-" + cid + ".scope\n"
	other := t.TempDir() + "/not-the-namespace"
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		netns, cgroup, want string
	}{
		"the container's":       {"/proc/self/ns/net", scope, ""},
		"another namespace":     {other, scope, "not the one mounted"},
		"another cgroup":        {"/proc/self/ns/net", "0::/user.slice/user-1000.slice/session-1.scope\n", "cgroup is not container"},
		"another container's":   {"/proc/self/ns/net", "0::/machine.slice/libpod-" + strings.Repeat("e", 64) + ".scope\n", "PID was reused"},
		"cgroupfs, another's":   {"/proc/self/ns/net", "0::/libpod_parent/libpod-" + strings.Repeat("e", 64) + "\n", "PID was reused"},
		"no cgroup at all":      {"/proc/self/ns/net", "0::/\n", "cgroup is not container"},
		"conmon's, not its own": {"/proc/self/ns/net", "0::/machine.slice/libpod-conmon-" + cid + ".scope\n", "cgroup is not container"},
	} {
		proc := fakeProcDir(t, pid, tc.netns, tc.cgroup)
		f, err := processBinder{proc: proc}.bind(pid, "/proc/self/ns/net", cid)
		if f != nil {
			f.Close()
		}
		expect(t, name, err, tc.want)
	}
}

// TestBindProcessExited: a PID with no process is refused.
func TestBindProcessExited(t *testing.T) {
	if _, err := BindProcess(1<<22+1, "/proc/self/ns/net", cid); err == nil {
		t.Error("a PID with no process was bound")
	}
}

// TestBindProcessReuseIsDistinct: only another container's cgroup is
// ErrPIDReused, which fails the start; not being in any libpod cgroup yet
// is not.
func TestBindProcessReuseIsDistinct(t *testing.T) {
	pid := sleeper(t)
	for cgroup, reused := range map[string]bool{
		"0::/machine.slice/libpod-" + strings.Repeat("e", 64) + ".scope\n": true,
		"0::/user.slice/user-1000.slice/session-1.scope\n":                 false,
		"0::/machine.slice/libpod-conmon-" + cid + ".scope\n":              false,
	} {
		f, err := processBinder{proc: fakeProcDir(t, pid, "/proc/self/ns/net", cgroup)}.bind(pid, "/proc/self/ns/net", cid)
		if f != nil {
			f.Close()
		}
		if err == nil || errors.Is(err, ErrPIDReused) != reused {
			t.Errorf("%q: %v, want reused %v", cgroup, err, reused)
		}
	}
}

func TestInContainerCgroup(t *testing.T) {
	for cgroup, want := range map[string]bool{
		"0::/machine.slice/libpod-" + cid + ".scope\n":                                                                    true,
		"0::/machine.slice/libpod-" + cid + ".scope/container\n":                                                          true,
		"0::/libpod_parent/libpod-" + cid + "\n":                                                                          true,
		"12:pids:/machine.slice/libpod-" + cid + ".scope\n1:name=systemd:/machine.slice/libpod-" + cid + ".scope\n0::/\n": true,
		"12:pids:/machine.slice/libpod-" + cid + ".scope\n1:name=systemd:/user.slice\n":                                   false,
		"0::/machine.slice/libpod-" + cid + "x.scope\n":                                                                   false,
		"":       false,
		"0::/\n": false,
	} {
		if got := inContainerCgroup(cgroup, cid); got != want {
			t.Errorf("inContainerCgroup(%q) = %v, want %v", cgroup, got, want)
		}
	}
}
