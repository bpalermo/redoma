package fdstore

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/bpalermo/redoma/common/sockettest"
)

// notifySocket listens where a test's Notify sends, as systemd would.
func notifySocket(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	path := filepath.Join(sockettest.Dir(t), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	sockettest.SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, path
}

// receive reads one notification and the descriptors that came with it.
func receive(t *testing.T, conn *net.UnixConn) (string, []int) {
	t.Helper()
	buf, oob := make([]byte, 4096), make([]byte, syscall.CmsgSpace(4*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		t.Fatal(err)
	}
	var fds []int
	for _, m := range msgs {
		got, err := syscall.ParseUnixRights(&m)
		if err != nil {
			t.Fatal(err)
		}
		fds = append(fds, got...)
	}
	return string(buf[:n]), fds
}

// TestAddAndRemove: what systemd's notify socket receives, over a real
// socket where the environment allows one.
func TestAddAndRemove(t *testing.T) {
	conn, path := notifySocket(t)
	addAndRemove(t, &Notify{Socket: path}, conn)
}

// TestAddAndRemoveUnconnected is TestAddAndRemove where socket(2) is
// refused (the agents' sandbox): a datagram socket pair, its receiving end
// bound to an abstract name and both ends then dissolved (connect with
// AF_UNSPEC), leaves a sender that is truly unconnected, as a new socket
// is. Notify sends on it exactly as in production, to the address it
// derives from Socket.
func TestAddAndRemoveUnconnected(t *testing.T) {
	sender, conn, name := unconnectedPair(t)
	n := &Notify{Socket: name, socket: func() (int, error) { return unix.Dup(sender) }}
	addAndRemove(t, n, conn)
}

// unconnectedPair is a datagram socket pair dissolved into an unconnected
// sender and a receiver bound to a fresh abstract name (@...).
func unconnectedPair(t *testing.T) (sender int, receiver *net.UnixConn, name string) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	name = "@redoma-fdstore-test-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := unix.Bind(fds[1], &unix.SockaddrUnix{Name: name}); err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		dissolve(t, fd)
	}
	t.Cleanup(func() { unix.Close(fds[0]) })
	f := os.NewFile(uintptr(fds[1]), "receiver")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return fds[0], c.(*net.UnixConn), name
}

// dissolve disconnects a datagram socket: connect(2) to AF_UNSPEC.
func dissolve(t *testing.T, fd int) {
	t.Helper()
	sa := unix.RawSockaddr{Family: unix.AF_UNSPEC}
	if _, _, errno := unix.Syscall(unix.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); errno != 0 {
		t.Fatalf("connect(AF_UNSPEC): %v", errno)
	}
	if _, err := unix.Getpeername(fd); err == nil {
		t.Fatal("still connected after connect(AF_UNSPEC)")
	}
}

func addAndRemove(t *testing.T, n *Notify, conn *net.UnixConn) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := n.Add("c0ffee", w); err != nil {
		t.Fatal(err)
	}
	state, fds := receive(t, conn)
	if state != "FDSTORE=1\nFDNAME=c0ffee\n" || len(fds) != 1 {
		t.Fatalf("Add sent %q with %d descriptors", state, len(fds))
	}
	stored := os.NewFile(uintptr(fds[0]), "stored")
	defer stored.Close()
	if _, err := stored.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil || buf[0] != 'x' {
		t.Errorf("the stored descriptor is not the pipe's: %q, %v", buf, err)
	}
	if err := n.Remove("c0ffee"); err != nil {
		t.Fatal(err)
	}
	if state, fds := receive(t, conn); state != "FDSTOREREMOVE=1\nFDNAME=c0ffee\n" || len(fds) != 0 {
		t.Errorf("Remove sent %q with %d descriptors", state, len(fds))
	}
}

func TestNoStore(t *testing.T) {
	n := &Notify{}
	if err := n.Remove("c0ffee"); !errors.Is(err, ErrNoStore) {
		t.Errorf("Remove without a socket: %v, want ErrNoStore", err)
	}
}

func TestValidName(t *testing.T) {
	for name, ok := range map[string]bool{
		"0123456789abcdef":        true,
		"":                        false,
		"a:b":                     false,
		"a b":                     false,
		"a\nFDSTORE=1":            false,
		string(make([]byte, 256)): false,
	} {
		if err := validName(name); (err == nil) != ok {
			t.Errorf("validName(%q) = %v", name, err)
		}
	}
}

// TestListened: the descriptors passed to this process, by name; several
// may share one; none for another process.
func TestListened(t *testing.T) {
	const start = 200
	for i := range 3 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
		if err := syscall.Dup2(int(r.Fd()), start+i); err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
	self := os.Getpid()
	files, err := listened(self, strconv.Itoa(self), "3", "a:b:a", start)
	if err != nil {
		t.Fatal(err)
	}
	if len(files["a"]) != 2 || len(files["b"]) != 1 {
		t.Errorf("listened = %v, want a twice and b once", files)
	}
	for _, fs := range files {
		for _, f := range fs {
			f.Close()
		}
	}
	if files, err := listened(self, strconv.Itoa(self+1), "3", "a:b:a", start); err != nil || len(files) != 0 {
		t.Errorf("another process's descriptors: %v, %v", files, err)
	}
	if _, err := listened(self, strconv.Itoa(self), "x", "", start); err == nil {
		t.Error("a LISTEN_FDS that is no count was accepted")
	}
}

// TestAbstractAddress: NOTIFY_SOCKET's @ names an abstract socket, whose
// address starts with a NUL on the wire: the receiver bound to Notify's
// address reports, raw, a NUL then the rest of the name, and gets what
// Notify sends.
func TestAbstractAddress(t *testing.T) {
	sender, conn, name := unconnectedPair(t)
	var raw unix.RawSockaddrUnix
	size := uint32(unsafe.Sizeof(raw))
	rc, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var errno unix.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = unix.Syscall(unix.SYS_GETSOCKNAME, fd, uintptr(unsafe.Pointer(&raw)), uintptr(unsafe.Pointer(&size)))
	}); err != nil || errno != 0 {
		t.Fatalf("getsockname: %v, %v", err, errno)
	}
	path := make([]byte, 0, len(name))
	for _, c := range raw.Path[:int(size)-2] {
		path = append(path, byte(c))
	}
	if len(path) == 0 || path[0] != 0 || string(path[1:]) != name[1:] {
		t.Errorf("the bound address is %q, want a NUL then %q", path, name[1:])
	}
	n := &Notify{Socket: name, socket: func() (int, error) { return unix.Dup(sender) }}
	if err := n.Remove("c0ffee"); err != nil {
		t.Fatal(err)
	}
	if state, _ := receive(t, conn); state != "FDSTOREREMOVE=1\nFDNAME=c0ffee\n" {
		t.Errorf("received %q", state)
	}
}
