// Package fdstore keeps descriptors in systemd's file descriptor store
// (sd_notify FDSTORE=1, FileDescriptorStoreMax= in the unit), so they
// outlive a restart of the process: systemd passes them back on the next
// start (LISTEN_FDS, LISTEN_FDNAMES).
//
// redoma xds keeps there the pidfd of each hook-registered agent's process,
// named after its container (hooksource).
package fdstore

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Store is where descriptors are kept by name. A name may hold several
// descriptors: Remove removes all of them.
type Store interface {
	// Add keeps f under name. The store has its own copy: f stays the
	// caller's.
	Add(name string, f *os.File) error
	// Remove drops every descriptor kept under name.
	Remove(name string) error
}

// ErrNoStore is a process systemd does not supervise with a notify socket
// (NOTIFY_SOCKET unset): nothing outlives it.
var ErrNoStore = errors.New("no systemd notify socket (NOTIFY_SOCKET): descriptors do not survive a restart")

// Notify is systemd's store, through the notify socket.
type Notify struct {
	// Socket is NOTIFY_SOCKET's value: a path, or @ and an abstract name.
	Socket string
	// socket is the datagram socket a message is sent on: a new one, as
	// sd_notify(3) makes, unless a test gives its own. Where it is sent is
	// always Socket's address (address).
	socket func() (fd int, err error)
}

var _ Store = (*Notify)(nil)

// Add sends FDSTORE=1 with f.
func (n *Notify) Add(name string, f *os.File) error {
	if err := validName(name); err != nil {
		return err
	}
	err := n.send("FDSTORE=1\nFDNAME="+name+"\n", syscall.UnixRights(int(f.Fd())))
	runtime.KeepAlive(f)
	return err
}

// Remove sends FDSTOREREMOVE=1.
func (n *Notify) Remove(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	return n.send("FDSTOREREMOVE=1\nFDNAME="+name+"\n", nil)
}

// send sends state, with oob's descriptors, in one datagram: sendmsg(2) on
// an unconnected socket, to the notify socket's address, as sd_notify(3)
// does. (Go's net refuses a WriteMsgUnix on a connected datagram socket,
// and its unconnected one needs a local address.)
func (n *Notify) send(state string, oob []byte) error {
	if n.Socket == "" {
		return ErrNoStore
	}
	open := n.socket
	if open == nil {
		open = newSocket
	}
	fd, err := open()
	if err != nil {
		return fmt.Errorf("systemd's notify socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Sendmsg(fd, []byte(state), oob, n.address(), 0); err != nil {
		return fmt.Errorf("systemd's notify socket %s: %w", n.Socket, err)
	}
	return nil
}

// newSocket is a new, unconnected datagram socket.
func newSocket() (int, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, os.NewSyscallError("socket", err)
	}
	return fd, nil
}

// address is the notify socket's. A leading @ names an abstract socket,
// which unix.SockaddrUnix writes as a leading NUL, as sd_notify(3) reads
// NOTIFY_SOCKET.
func (n *Notify) address() *unix.SockaddrUnix {
	return &unix.SockaddrUnix{Name: n.Socket}
}

// validName is what systemd takes as an FDNAME: at most 255 characters,
// printable, and no colon (LISTEN_FDNAMES separates the names with them).
func validName(name string) error {
	if name == "" || len(name) > 255 {
		return fmt.Errorf("descriptor name %q: empty or over 255 characters", name)
	}
	for _, r := range name {
		if r == ':' || r < 0x21 || r > 0x7e {
			return fmt.Errorf("descriptor name %q: not printable ASCII without a colon", name)
		}
	}
	return nil
}

// listenFDsStart is the first descriptor systemd passes (SD_LISTEN_FDS_START).
const listenFDsStart = 3

// FromEnv is the store of a process systemd started: the notify socket,
// and the descriptors systemd passed back from it, by name. It unsets the
// variables, so no child inherits them, and marks the descriptors
// close-on-exec. Descriptors passed to another process (LISTEN_PID not
// this one) are ignored.
func FromEnv() (*Notify, map[string][]*os.File, error) {
	n := &Notify{Socket: os.Getenv("NOTIFY_SOCKET")}
	pid, count, names := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), os.Getenv("LISTEN_FDNAMES")
	for _, v := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		os.Unsetenv(v)
	}
	files, err := listened(os.Getpid(), pid, count, names, listenFDsStart)
	return n, files, err
}

// listened reads the LISTEN_* variables for the process self, whose
// descriptors start at start.
func listened(self int, pid, count, names string, start int) (map[string][]*os.File, error) {
	files := map[string][]*os.File{}
	if count == "" || pid != strconv.Itoa(self) {
		return files, nil
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 {
		return files, fmt.Errorf("LISTEN_FDS %q: not a count", count)
	}
	split := strings.Split(names, ":")
	for i := range n {
		fd := start + i
		syscall.CloseOnExec(fd)
		name := "unknown"
		if i < len(split) && split[i] != "" {
			name = split[i]
		}
		files[name] = append(files[name], os.NewFile(uintptr(fd), "stored:"+name))
	}
	return files, nil
}
