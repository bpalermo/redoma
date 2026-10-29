package reaper

import (
	"context"
	"log/slog"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestKillEmptiesTheSubtree: a turn that ignores SIGTERM and keeps forking
// is gone, children and all, once Kill returns, and the dead are reaped.
func TestKillEmptiesTheSubtree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := New(ctx, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `trap "" TERM; while :; do sleep 60 & sleep 0.01; done`)
	if err := r.Start(cmd); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		r.Done(cmd.Process.Pid)
		close(waited)
	}()
	time.Sleep(200 * time.Millisecond)
	if n := len(r.descendants()); n < 2 {
		t.Fatalf("%d processes below the test, want the shell and its sleeps", n)
	}
	if err := r.Kill(100 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if left := r.descendants(); len(left) != 0 {
		t.Errorf("left after Kill: %v", left)
	}
	<-waited
	deadline := time.Now().Add(5 * time.Second)
	for {
		zombies := 0
		for _, p := range r.procs() {
			if p.ppid == r.self && p.state == 'Z' {
				zombies++
			}
		}
		if zombies == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d zombies not reaped", zombies)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestKillWithNothing: with nothing below the shim, Kill returns at once.
func TestKillWithNothing(t *testing.T) {
	r := &Reaper{log: slog.New(slog.DiscardHandler), proc: "/proc", self: -1, managed: map[int]bool{}}
	start := time.Now()
	if err := r.Kill(time.Hour); err != nil || time.Since(start) > time.Second {
		t.Errorf("Kill = %v after %v", err, time.Since(start))
	}
}

// TestKillWithoutPidfds: where pidfd_open fails (EPERM from a seccomp
// filter), every process is still signalled, by pid.
func TestKillWithoutPidfds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := New(ctx, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	r.pidfdOpen = func(int) (int, syscall.Errno) { return -1, syscall.EPERM }
	cmd := exec.Command("/bin/sh", "-c", `trap "" TERM; sleep 60 & wait`)
	if err := r.Start(cmd); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		r.Done(cmd.Process.Pid)
		close(waited)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := r.Kill(100 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if left := r.descendants(); len(left) != 0 {
		t.Errorf("left after Kill without pidfds: %v", left)
	}
	<-waited
}

// counter counts the warnings logged through it.
type counter struct {
	slog.Handler
	n *atomic.Int32
}

func (c counter) Enabled(context.Context, slog.Level) bool { return true }

func (c counter) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		c.n.Add(1)
	}
	return nil
}

// TestNoPidfdWarnsOnce: signalling by pid is said once per errno, not for
// every process at every scan.
func TestNoPidfdWarnsOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var n atomic.Int32
	r, err := New(ctx, slog.New(counter{Handler: slog.DiscardHandler, n: &n}))
	if err != nil {
		t.Fatal(err)
	}
	r.pidfdOpen = func(int) (int, syscall.Errno) { return -1, syscall.EPERM }
	for range 3 {
		cmd := exec.Command("/bin/sh", "-c", `sleep 60 & sleep 60 & wait`)
		if err := r.Start(cmd); err != nil {
			t.Skipf("no /bin/sh: %v", err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); r.Done(cmd.Process.Pid); close(done) }()
		time.Sleep(50 * time.Millisecond)
		if err := r.Kill(100 * time.Millisecond); err != nil {
			t.Fatal(err)
		}
		<-done
	}
	if got := n.Load(); got != 1 {
		t.Errorf("%d warnings for 3 kills of 3 processes each, want 1", got)
	}
}
