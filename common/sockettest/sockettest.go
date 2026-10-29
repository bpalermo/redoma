// Package sockettest is for the tests of real unix sockets: they skip where
// the environment refuses socket(2) (the agents' sandbox does), and fail
// there instead when CI is set (--config=ci passes it), so CI never skips
// them.
package sockettest

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Dir is a fresh directory for sockets. A socket path is limited to 107
// bytes, which Bazel's test temporary directory can exceed, so the directory
// is made under /tmp (private to the test's sandbox) when it can be.
func Dir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "redoma-")
	if err != nil {
		return t.TempDir()
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// ListenUnix listens on a unix socket named name in a fresh Dir, and skips
// the test where the environment forbids unix sockets.
func ListenUnix(t testing.TB, name string) (net.Listener, string) {
	t.Helper()
	path := filepath.Join(Dir(t), name)
	l, err := net.Listen("unix", path)
	SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	return l, path
}

// UnixSocketsRefused reports whether err is the socket(2) call itself being
// refused for AF_UNIX, as a seccomp sandbox does. Only that is a reason to
// skip: a bind, chmod or permission error on the path is a failure.
func UnixSocketsRefused(err error) bool {
	var sys *os.SyscallError
	if !errors.As(err, &sys) || sys.Syscall != "socket" {
		return false
	}
	return errors.Is(sys.Err, syscall.EPERM) || errors.Is(sys.Err, syscall.EACCES) || errors.Is(sys.Err, syscall.EAFNOSUPPORT)
}

// SkipIfRefused skips the test when err is a refused socket(2), except in CI
// (CI set, which .bazelrc passes to tests under --config=ci), where every
// socket test has to run: there it fails instead.
func SkipIfRefused(t testing.TB, err error) {
	t.Helper()
	if !UnixSocketsRefused(err) {
		return
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("unix sockets are refused, and CI must run this test: %v", err)
	}
	t.Skipf("this environment forbids unix sockets: %v", err)
}
