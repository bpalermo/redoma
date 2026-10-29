package bridge

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bpalermo/redoma/common/sockettest"
)

type lead struct {
	cancelled chan struct{}
	messages  chan string
}

func (l *lead) Ask(ctx context.Context, q string) (string, error) {
	switch q {
	case "fail":
		return "", errors.New("no turn is running")
	case "wait":
		<-ctx.Done()
		close(l.cancelled)
		return "", ctx.Err()
	}
	return "re: " + q, nil
}

func (l *lead) Message(_ context.Context, text string) error {
	l.messages <- text
	return nil
}

func serve(t *testing.T, l net.Listener) (*lead, string) {
	t.Helper()
	ld := &lead{cancelled: make(chan struct{}), messages: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, l, ld) }()
	return ld, Addr(l)
}

func exercise(t *testing.T, ld *lead, addr string) {
	t.Helper()
	ctx := context.Background()
	if got, err := Ask(ctx, addr, "which?"); err != nil || got != "re: which?" {
		t.Errorf("Ask = %q, %v", got, err)
	}
	if _, err := Ask(ctx, addr, "fail"); err == nil || err.Error() != "no turn is running" {
		t.Errorf("a failed ask: %v", err)
	}
	if err := Message(ctx, addr, "fyi"); err != nil || <-ld.messages != "fyi" {
		t.Errorf("Message: %v", err)
	}
	// A caller that goes away ends its request on the shim's side.
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := Ask(cctx, addr, "wait"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an abandoned ask: %v", err)
	}
	select {
	case <-ld.cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the shim kept waiting for a caller that left")
	}
}

// TestTCP: loopback TCP, which the tests' sandbox allows.
func TestTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ld, addr := serve(t, l)
	exercise(t, ld, addr)
}

// TestUnix: the daemon's unix socket.
func TestUnix(t *testing.T) {
	l, err := net.Listen("unix", filepath.Join(sockettest.Dir(t), "shim.sock"))
	sockettest.SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	ld, addr := serve(t, l)
	exercise(t, ld, addr)
}

func TestBadAddress(t *testing.T) {
	if _, err := Ask(context.Background(), "http://x", "q"); err == nil {
		t.Error("asked a bad address")
	}
}

// flaky fails its first Accepts as a full descriptor table would.
type flaky struct {
	net.Listener
	fails int
}

func (f *flaky) Accept() (net.Conn, error) {
	if f.fails > 0 {
		f.fails--
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return f.Listener.Accept()
}

// TestAcceptRetries: an accept error that is not the listener closing does
// not end the bridge.
func TestAcceptRetries(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ld, addr := serve(t, &flaky{Listener: l, fails: 3})
	exercise(t, ld, addr)
}
