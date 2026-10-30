package fdstore

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

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

// TestAddAndRemoveOverAPair is TestAddAndRemove over a socket pair, which
// the agents' sandbox allows.
func TestAddAndRemoveOverAPair(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socket")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		conns[i] = c.(*net.UnixConn)
	}
	// The pair's end is closed after each send, as a dialled connection
	// is: hand out a duplicate.
	dial := func() (*net.UnixConn, error) {
		f, err := conns[0].File()
		if err != nil {
			return nil, err
		}
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			return nil, err
		}
		return c.(*net.UnixConn), nil
	}
	addAndRemove(t, &Notify{dial: dial}, conns[1])
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
