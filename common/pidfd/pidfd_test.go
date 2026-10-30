package pidfd

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestMain is also the child the tests hold: with REDOMA_PIDFD_CHILD set it
// waits until its stdin closes.
func TestMain(m *testing.M) {
	if os.Getenv("REDOMA_PIDFD_CHILD") != "" {
		_, _ = os.Stdin.Read(make([]byte, 1))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// child starts a process that runs until stop is called, which waits for
// it (reaps it).
func child(t *testing.T) (cmd *exec.Cmd, stop func()) {
	t.Helper()
	cmd = exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "REDOMA_PIDFD_CHILD=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			in.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	return cmd, stop
}

// TestAliveThenExited: the pidfd names the child while it runs, then
// reports it exited, and its PID gone once it is reaped.
func TestAliveThenExited(t *testing.T) {
	cmd, stop := child(t)
	f, err := Open(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Alive() {
		t.Error("Alive = false for a running process")
	}
	if pid, err := p.PID(); err != nil || pid != cmd.Process.Pid {
		t.Errorf("PID = %d, %v; want %d", pid, err, cmd.Process.Pid)
	}
	stop()
	if p.Alive() {
		t.Error("Alive = true for a reaped process")
	}
	if _, err := p.PID(); !errors.Is(err, ErrExited) {
		t.Errorf("PID of a reaped process: %v, want ErrExited", err)
	}
}

// TestZombieIsExited: a process that has exited but is not reaped still
// holds its PID, and counts as exited.
func TestZombieIsExited(t *testing.T) {
	cmd, _ := child(t)
	f, err := Open(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Kill it without waiting: a zombie until the test's cleanup reaps it.
	if err := cmd.Process.Signal(os.Kill); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for p.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("the killed process is still alive")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestClosedIsExited: a closed FD is never alive, whatever took its number.
func TestClosedIsExited(t *testing.T) {
	cmd, _ := child(t)
	f, err := Open(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if p.Alive() {
		t.Error("Alive = true after Close")
	}
	if _, err := p.PID(); err == nil {
		t.Error("PID after Close: no error")
	}
}

// TestNewRefusesAnotherDescriptor: a file that is not a pidfd is refused,
// and closed.
func TestNewRefusesAnotherDescriptor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := New(r); err == nil {
		t.Fatal("New(a pipe) accepted it")
	}
	if _, err := r.Stat(); err == nil {
		t.Error("the refused file is still open")
	}
}

func TestParsePid(t *testing.T) {
	for in, want := range map[string]int{
		"pos:\t0\nflags:\t02000002\nmnt_id:\t15\nino:\t1057\nPid:\t4242\nNSpid:\t4242\n": 4242,
		"Pid:\t-1\n": -1,
	} {
		if got, err := parsePid([]byte(in)); err != nil || got != want {
			t.Errorf("parsePid(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"pos:\t0\nflags:\t02\n", "Pid:\tx\n"} {
		if _, err := parsePid([]byte(in)); err == nil {
			t.Errorf("parsePid(%q): no error", in)
		}
	}
}
