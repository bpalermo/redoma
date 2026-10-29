package relay

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/bpalermo/redoma/common/sockettest"
)

func owner(t *testing.T, path string) (uid, gid int, mode os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), fi.Mode()
}

// TestListen: the socket replaces a stale one, is ours, has the group it
// was given and SocketMode, and accepts.
func TestListen(t *testing.T) {
	path := filepath.Join(sockettest.Dir(t), "relay.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := listen(path, os.Getgid())
	sockettest.SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	uid, gid, mode := owner(t, path)
	if uid != os.Getuid() || gid != os.Getgid() || mode&os.ModeSocket == 0 || mode.Perm() != SocketMode {
		t.Errorf("%v %d:%d, want a socket %o %d:%d", mode, uid, gid, SocketMode, os.Getuid(), os.Getgid())
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// TestListenSetsTheGroup: in a setgid directory of another group (as a
// /run/redoma-relay of group redoma-lead would be), each socket still has
// the group it was given: the agents' socket never the lead's group. Needs
// a supplementary group to play the other one.
func TestListenSetsTheGroup(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	other := -1
	for _, g := range groups {
		if g != os.Getgid() {
			other = g
		}
	}
	if other == -1 {
		t.Skip("no supplementary group to play the lead's")
	}
	dir := sockettest.Dir(t)
	if err := os.Chown(dir, -1, other); err != nil {
		// A sandbox's user namespace (Bazel's own included) may map none
		// of the supplementary groups.
		t.Skipf("cannot give a directory our supplementary group %d here: %v", other, err)
	}
	if err := os.Chmod(dir, 0o2755); err != nil {
		t.Fatal(err)
	}
	agents, err := listen(filepath.Join(dir, "relay.sock"), os.Getgid())
	sockettest.SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	defer agents.Close()
	lead, err := listen(filepath.Join(dir, "lead.sock"), other)
	if err != nil {
		t.Fatal(err)
	}
	defer lead.Close()
	if _, gid, mode := owner(t, filepath.Join(dir, "relay.sock")); gid != os.Getgid() || mode.Perm() != SocketMode {
		t.Errorf("relay.sock %v group %d, want %o group %d (not the directory's %d)", mode, gid, SocketMode, os.Getgid(), other)
	}
	if _, gid, mode := owner(t, filepath.Join(dir, "lead.sock")); gid != other || mode.Perm() != SocketMode {
		t.Errorf("lead.sock %v group %d, want %o group %d", mode, gid, SocketMode, other)
	}
}

// TestSetOwnerChecks: what the chown and chmod left is checked: a path that
// is not a socket is refused even when both succeed.
func TestSetOwnerChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setOwner(path, os.Getuid(), os.Getgid()); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("setOwner on a regular file: %v, want refused", err)
	}
	if err := setOwner(path, os.Getuid(), 1<<30); err == nil {
		t.Error("setOwner to a group we are not in: accepted")
	}
}

// TestSameGroupsRefused: the agents' socket and the lead's must not share a
// group; the relay refuses to start before it opens anything.
func TestSameGroupsRefused(t *testing.T) {
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skipf("our own group has no name: %v", err)
	}
	dir := t.TempDir()
	f := flags{
		socket: filepath.Join(dir, "relay.sock"), socketGroup: g.Name,
		leadSocket: filepath.Join(dir, "lead", "lead.sock"), leadGroup: g.Name,
		stateDir: filepath.Join(dir, "state"),
	}
	err = run(context.Background(), f, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "both gid") {
		t.Errorf("run with one group for both sockets: %v, want refused", err)
	}
	if _, err := os.Stat(f.stateDir); err == nil {
		t.Error("the state directory was made before the refusal")
	}
}

// TestSetDir: a socket's directory gets the socket's group and DirMode,
// losing a setgid bit; a symlink or a file is refused.
func TestSetDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "redoma-relay-lead")
	if err := os.Mkdir(dir, 0o2777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o2777); err != nil {
		t.Fatal(err)
	}
	if err := setDir(dir, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	_, gid, mode := owner(t, dir)
	if gid != os.Getgid() || mode.Perm() != DirMode || mode&os.ModeSetgid != 0 {
		t.Errorf("%v group %d, want %o group %d", mode, gid, DirMode, os.Getgid())
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := setDir(link, os.Getgid()); err == nil {
		t.Error("a symlinked directory: accepted")
	}
}

// TestLeadSocketApart: the lead's socket may not share the agents' socket's
// directory, which the Envoy container mounts; refused before anything.
func TestLeadSocketApart(t *testing.T) {
	dir := t.TempDir()
	f := flags{
		socket: filepath.Join(dir, "relay.sock"), socketGroup: "redoma-relay",
		leadSocket: filepath.Join(dir, "lead.sock"), leadGroup: "redoma-lead",
		stateDir: filepath.Join(dir, "state"),
	}
	err := run(context.Background(), f, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "one directory") {
		t.Errorf("run with both sockets in one directory: %v, want refused", err)
	}
}

// TestSameDir: one directory by two paths (a symlink, as /var/run is /run)
// is one directory.
func TestSameDir(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "run")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "var-run")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameDir(real, link) {
		t.Error("a directory and a symlink to it: not the same")
	}
	if !sameDir(filepath.Join(dir, "missing"), filepath.Join(dir, "x", "..", "missing")) {
		t.Error("two spellings of a missing path: not the same")
	}
	if sameDir(real, dir) {
		t.Error("two directories: the same")
	}

	// run refuses them before anything.
	f := flags{
		socket: filepath.Join(real, "relay.sock"), socketGroup: "redoma-relay",
		leadSocket: filepath.Join(link, "lead.sock"), leadGroup: "redoma-lead",
		stateDir: filepath.Join(dir, "state"),
	}
	if err := run(context.Background(), f, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "one directory") {
		t.Errorf("the lead socket in the agents' directory by a symlink: %v, want refused", err)
	}
}
