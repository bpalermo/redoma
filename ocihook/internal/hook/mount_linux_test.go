package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// fakeProc is a /proc with plain files for namespaces: /proc/1/ns/net and
// /proc/<pid>/ns/net, the same file when sameAsHost.
func fakeProc(t *testing.T, pid int, sameAsHost bool) string {
	t.Helper()
	proc := t.TempDir()
	for _, p := range []int{1, pid} {
		if err := os.MkdirAll(filepath.Join(proc, fmt.Sprint(p), "ns"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	host := filepath.Join(proc, "1", "ns", "net")
	if err := os.WriteFile(host, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(proc, fmt.Sprint(pid), "ns", "net")
	if sameAsHost {
		if err := os.Link(host, mine); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(mine, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	return proc
}

// notMounted is umount2 on a path that is not a mount point.
func notMounted(string) error { return syscall.EINVAL }

// TestUnmountErrors: a target that is not mounted, or not there, is fine and
// is removed; any other umount error, EPERM included, is returned and the
// target is kept.
func TestUnmountErrors(t *testing.T) {
	for errno, wantErr := range map[syscall.Errno]bool{
		syscall.EINVAL: false,
		syscall.ENOENT: false,
		syscall.EPERM:  true,
		syscall.EBUSY:  true,
	} {
		target := filepath.Join(t.TempDir(), "redoma-oci-x")
		if err := os.WriteFile(target, nil, 0o444); err != nil {
			t.Fatal(err)
		}
		m := SystemMounter{Umount: func(string) error { return errno }}
		err := m.Unmount(target)
		if (err != nil) != wantErr {
			t.Errorf("%v: Unmount = %v, want error %v", errno, err, wantErr)
		}
		_, statErr := os.Stat(target)
		if kept := statErr == nil; kept != wantErr {
			t.Errorf("%v: target kept = %v, want %v", errno, kept, wantErr)
		}
	}
}

// TestBindNetnsMountsTheFileItChecked: the namespace is opened once, and
// what is mounted is that open file (/proc/self/fd/N), not the path again.
func TestBindNetnsMountsTheFileItChecked(t *testing.T) {
	proc := fakeProc(t, 4242, false)
	target := filepath.Join(t.TempDir(), "redoma-oci-x")
	var opened []string
	var fds []uintptr
	var src string
	m := SystemMounter{
		Proc:   proc,
		Umount: notMounted,
		Open: func(path string) (*os.File, error) {
			f, err := os.Open(path)
			if err == nil {
				opened, fds = append(opened, path), append(fds, f.Fd())
			}
			return f, err
		},
		Mount: func(s, _ string) error {
			src = s
			return nil
		},
	}
	if err := m.BindNetns(4242, target); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 2 || opened[0] != filepath.Join(proc, "4242", "ns", "net") {
		t.Fatalf("opened %q; want the container's namespace, then the host's", opened)
	}
	if want := fmt.Sprintf("/proc/self/fd/%d", fds[0]); src != want {
		t.Errorf("mounted %q, want the checked file %q", src, want)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("no mount target: %v", err)
	}
}

// TestBindNetnsRefusesTheHost: the container's namespace file is the
// host's (the same inode): refused, nothing mounted.
func TestBindNetnsRefusesTheHost(t *testing.T) {
	proc := fakeProc(t, 4242, true)
	mounted := false
	m := SystemMounter{Proc: proc, Umount: notMounted, Mount: func(string, string) error { mounted = true; return nil }}
	target := filepath.Join(t.TempDir(), "redoma-oci-x")
	if err := m.BindNetns(4242, target); !errors.Is(err, ErrHostNetns) {
		t.Fatalf("BindNetns = %v, want ErrHostNetns", err)
	}
	if mounted {
		t.Error("the host's namespace was mounted")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("a mount target was created")
	}
}
