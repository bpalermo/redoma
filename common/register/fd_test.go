package register

import (
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// pair is a connected pair of unix stream sockets.
func pair(t *testing.T) (a, b *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "socket")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c.(*net.UnixConn)
	}
	return conn(fds[0]), conn(fds[1])
}

// TestRequestWithAFile: the descriptor arrives with the request, as a new
// descriptor of the same file.
func TestRequestWithAFile(t *testing.T) {
	a, b := pair(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	in := Request{Op: OpRegister, Container: strings.Repeat("ab", 32), Agent: "a", Netns: "/run/netns/x", Pidfd: true}
	if err := WriteRequest(a, in, w); err != nil {
		t.Fatal(err)
	}
	out, f, err := ReadRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if out != in {
		t.Errorf("read %+v, want %+v", out, in)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil || buf[0] != 'x' {
		t.Errorf("the received descriptor is not the pipe's: %q, %v", buf, err)
	}
}

// TestRequestWithoutAFile: a plain request over a unix socket, and over any
// other connection.
func TestRequestWithoutAFile(t *testing.T) {
	a, b := pair(t)
	in := Request{Op: OpUnregister, Container: strings.Repeat("ab", 32)}
	if err := WriteRequest(a, in, nil); err != nil {
		t.Fatal(err)
	}
	if out, f, err := ReadRequest(b); err != nil || f != nil || out != in {
		t.Errorf("ReadRequest = %+v, %v, %v", out, f, err)
	}
	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()
	go func() { _ = Write(c, in) }()
	if out, f, err := ReadRequest(d); err != nil || f != nil || out != in {
		t.Errorf("ReadRequest over a pipe = %+v, %v, %v", out, f, err)
	}
}

// TestRequestRefusesAMismatch: a pidfd announced and not sent, or sent and
// not announced, is refused.
func TestRequestRefusesAMismatch(t *testing.T) {
	cid := strings.Repeat("ab", 32)
	t.Run("announced, not sent", func(t *testing.T) {
		a, b := pair(t)
		if err := Write(a, Request{Op: OpRegister, Container: cid, Pidfd: true}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadRequest(b); err == nil || !strings.Contains(err.Error(), "announces a pidfd") {
			t.Errorf("ReadRequest = %v", err)
		}
	})
	t.Run("sent, not announced", func(t *testing.T) {
		a, b := pair(t)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		data := []byte(`{"op":"register","container":"` + cid + `"}` + "\n")
		if _, _, err := a.WriteMsgUnix(data, syscall.UnixRights(int(w.Fd())), nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadRequest(b); err == nil || !strings.Contains(err.Error(), "does not announce") {
			t.Errorf("ReadRequest = %v", err)
		}
	})
	t.Run("not over a unix socket", func(t *testing.T) {
		c, d := net.Pipe()
		defer c.Close()
		defer d.Close()
		go func() { _ = Write(c, Request{Op: OpRegister, Container: cid, Pidfd: true}) }()
		if _, _, err := ReadRequest(d); err == nil {
			t.Error("a pidfd announced over a pipe was accepted")
		}
	})
	t.Run("a file with no announcement", func(t *testing.T) {
		a, _ := pair(t)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		if err := WriteRequest(a, Request{Op: OpRegister, Container: cid}, w); err == nil {
			t.Error("WriteRequest sent a descriptor the request does not announce")
		}
	})
}

// TestRequestRefusesExtraDescriptors: a pidfd announced and two sent, or
// more than ReadRequest makes room for (the kernel truncates the control
// data: MSG_CTRUNC), is refused, and what was received is closed.
func TestRequestRefusesExtraDescriptors(t *testing.T) {
	cid := strings.Repeat("ab", 32)
	data := []byte(`{"op":"register","container":"` + cid + `","pidfd":true}` + "\n")
	for name, tc := range map[string]struct {
		count int
		want  string
	}{
		"two":           {2, "came with 2 descriptors"},
		"over the room": {maxFiles + 4, "too many descriptors"},
	} {
		a, b := pair(t)
		var fds []int
		var files []*os.File
		for range tc.count {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, r, w)
			fds = append(fds, int(w.Fd()))
		}
		if _, _, err := a.WriteMsgUnix(data, syscall.UnixRights(fds...), nil); err != nil {
			t.Fatal(err)
		}
		if _, f, err := ReadRequest(b); err == nil || f != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ReadRequest = %v, %v; want %q", name, f, err, tc.want)
		}
		for _, f := range files {
			f.Close()
		}
	}
}
