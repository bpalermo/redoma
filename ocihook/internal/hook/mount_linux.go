package hook

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// ErrHostNetns is BindNetns refusing the host's network namespace.
var ErrHostNetns = errors.New("the container is in the host's network namespace (--network host): an agent needs its own")

// SystemMounter mounts with mount(2). It needs CAP_SYS_ADMIN in the host's
// mount namespace: the runtime runs createRuntime and poststop hooks there,
// as root, for rootful containers.
type SystemMounter struct {
	// Open opens a namespace file; nil is os.Open. Tests open plain files.
	Open func(path string) (*os.File, error)
	// Mount bind-mounts src at target; nil is mount(2) with MS_BIND.
	Mount func(src, target string) error
	// Umount detaches the mount at a path; nil is umount2(2) with
	// MNT_DETACH.
	Umount func(target string) error
	// Proc is the procfs root; "" is /proc.
	Proc string
}

func (m SystemMounter) open(path string) (*os.File, error) {
	if m.Open != nil {
		return m.Open(path)
	}
	return os.Open(path)
}

func (m SystemMounter) mount(src, target string) error {
	if m.Mount != nil {
		return m.Mount(src, target)
	}
	return syscall.Mount(src, target, "", syscall.MS_BIND, "")
}

func (m SystemMounter) proc() string {
	if m.Proc != "" {
		return m.Proc
	}
	return "/proc"
}

// BindNetns implements Mounter, without a race on the path: pid's
// namespace file is opened once, that open file is what is compared with
// the host's (pid 1's) and what is mounted, through /proc/self/fd, so a
// pid that exits and is reused between the check and the mount cannot
// swap in another namespace.
func (m SystemMounter) BindNetns(pid int, target string) error {
	ns, err := m.open(fmt.Sprintf("%s/%d/ns/net", m.proc(), pid))
	if err != nil {
		return err
	}
	defer ns.Close()
	host, err := m.open(m.proc() + "/1/ns/net")
	if err != nil {
		return err
	}
	same, err := sameFile(ns, host)
	host.Close()
	if err != nil {
		return err
	}
	if same {
		return ErrHostNetns
	}
	// A stale mount from a crash of an earlier container with this ID
	// cannot exist (IDs are not reused), but one from a hook that died
	// between mount and register can: replace it.
	if err := m.unmount(target); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	src := fmt.Sprintf("/proc/self/fd/%d", ns.Fd())
	if err := m.mount(src, target); err != nil {
		os.Remove(target)
		return fmt.Errorf("mount --bind %s %s: %w", src, target, err)
	}
	return nil
}

// sameFile reports whether two open files are the same inode of the same
// device: for nsfs files, the same namespace.
func sameFile(a, b *os.File) (bool, error) {
	af, err := a.Stat()
	if err != nil {
		return false, err
	}
	bf, err := b.Stat()
	if err != nil {
		return false, err
	}
	as, aok := af.Sys().(*syscall.Stat_t)
	bs, bok := bf.Sys().(*syscall.Stat_t)
	if !aok || !bok {
		return false, errors.New("no device and inode")
	}
	return as.Dev == bs.Dev && as.Ino == bs.Ino, nil
}

// Unmount implements Mounter.
func (m SystemMounter) Unmount(target string) error {
	return m.unmount(target)
}

// unmount detaches the mount at target, if any, and removes the file.
func (m SystemMounter) unmount(target string) error {
	umount := m.Umount
	if umount == nil {
		umount = func(t string) error { return syscall.Unmount(t, syscall.MNT_DETACH) }
	}
	err := umount(target)
	// EINVAL: not a mount point; ENOENT: nothing there. Anything else, EPERM
	// included, is an error: the hook runs as root.
	if err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("umount %s: %w", target, err)
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
