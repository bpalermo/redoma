// Package reaper makes the shim a proper init for its container: it is PID 1
// (the image's entrypoint), so every process a turn leaves behind is
// reparented to it, and once dead stays a zombie unless it is reaped.
//
// The shim is made a child subreaper (PR_SET_CHILD_SUBREAPER), so that holds
// even where it is not PID 1, and a goroutine reaps, on every SIGCHLD and
// every second, each zombie child that is not one the shim waits for itself
// (Start registers those, and exec.Cmd.Wait reaps them, with their status).
// Zombies are found in /proc, by state and parent, rather than with
// wait4(-1), which would take the turn's own exit status.
//
// Kill ends a whole subtree: the turn is every process below the shim (the
// shim runs one turn at a time and starts nothing else), whatever its
// process group or session, so a descendant that called setsid goes too.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const prSetChildSubreaper = 36

// Reaper reaps the shim's zombie children. Make it with New.
type Reaper struct {
	log  *slog.Logger
	proc string
	self int

	mu      sync.Mutex
	managed map[int]bool

	// warned are the pidfd_open errors already logged (warnNoPidfd).
	warned map[syscall.Errno]bool
	// pidfdOpen is pidfd_open(2) unless a test fails it.
	pidfdOpen func(pid int) (int, syscall.Errno)
}

// warnNoPidfd says, once per errno, that processes are signalled by pid.
func (r *Reaper) warnNoPidfd(errno syscall.Errno) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned[errno] {
		return
	}
	if r.warned == nil {
		r.warned = map[syscall.Errno]bool{}
	}
	r.warned[errno] = true
	r.log.Warn("no pidfd for the turn's processes; signalling them by pid (said once)", "err", errno)
}

func (r *Reaper) open(pid int) (int, syscall.Errno) {
	if r.pidfdOpen != nil {
		return r.pidfdOpen(pid)
	}
	fd, _, errno := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	return int(fd), errno
}

// New makes the calling process a child subreaper and reaps until ctx ends.
func New(ctx context.Context, log *slog.Logger) (*Reaper, error) {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return nil, fmt.Errorf("prctl(PR_SET_CHILD_SUBREAPER): %w", errno)
	}
	r := &Reaper{log: log, proc: "/proc", self: os.Getpid(), managed: map[int]bool{}}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	go func() {
		defer signal.Stop(sigs)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigs:
			case <-tick.C:
			}
			r.Reap()
		}
	}()
	return r, nil
}

// Start starts cmd as a child the caller reaps (with cmd.Wait); Done ends
// that once it has.
func (r *Reaper) Start(cmd *exec.Cmd) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	r.managed[cmd.Process.Pid] = true
	return nil
}

// Done says the caller has reaped pid.
func (r *Reaper) Done(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.managed, pid)
}

// proc is one process's state from /proc/<pid>/stat.
type proc struct {
	pid, ppid int
	state     byte
}

// procs reads every process's pid, parent and state.
func (r *Reaper) procs() []proc {
	entries, err := os.ReadDir(r.proc)
	if err != nil {
		return nil
	}
	var out []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r.proc, e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...; comm may hold spaces and parens.
		s := string(raw)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 || len(f[0]) != 1 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		out = append(out, proc{pid: pid, ppid: ppid, state: f[0][0]})
	}
	return out
}

// Reap reaps every zombie child the shim does not wait for itself, and
// returns how many.
func (r *Reaper) Reap() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.procs() {
		if p.ppid != r.self || p.state != 'Z' || r.managed[p.pid] {
			continue
		}
		var ws syscall.WaitStatus
		if got, err := syscall.Wait4(p.pid, &ws, syscall.WNOHANG, nil); err == nil && got == p.pid {
			n++
		}
	}
	return n
}

// descendants are the live processes below the shim, deepest last.
func (r *Reaper) descendants() []int {
	children := map[int][]int{}
	live := map[int]bool{}
	for _, p := range r.procs() {
		children[p.ppid] = append(children[p.ppid], p.pid)
		live[p.pid] = p.state != 'Z' && p.state != 'X'
	}
	var out []int
	queue := []int{r.self}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if live[c] {
				out = append(out, c)
			}
			queue = append(queue, c)
		}
	}
	return out
}

// killTimeout bounds how long Kill goes on sending SIGKILL to what is
// still below the shim.
const killTimeout = 5 * time.Second

// Kill ends every process below the shim: SIGTERM, then, after grace,
// SIGKILL to whatever is still there, scanning again and again until
// nothing is (a process forked between a scan and its signals is caught by
// the next scan). Each signal goes through a pidfd to a process found below
// the shim after the pidfd was opened (signalAll). It fails when
// something is still there after killTimeout of SIGKILLs: a process stuck
// in the kernel, which the caller must not start another turn beside.
func (r *Reaper) Kill(grace time.Duration) error {
	signalAll := func(sig syscall.Signal) int { return r.signalAll(sig) }
	if signalAll(syscall.SIGTERM) == 0 {
		return nil
	}
	if r.wait(grace) {
		return nil
	}
	deadline := time.Now().Add(killTimeout)
	for signalAll(syscall.SIGKILL) > 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("processes of the turn survive SIGKILL: %v", r.descendants())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// The pidfd system calls, the same number on amd64 and arm64.
const (
	sysPidfdSendSignal = 424
	sysPidfdOpen       = 434
)

// signalAll signals every process below the shim and returns how many it
// found. Each is pinned by a pidfd first, and signalled only if a scan made
// after the pidfd was opened still finds it below the shim: a pid freed
// (reaped) and reused between the first scan and the open is then another
// process's, not below the shim, and is left alone, and a pidfd once open
// can only ever signal the process it was opened on. Where pidfds are not
// there (Linux before 5.3), it falls back to kill(2), with the race that
// carries.
func (r *Reaper) signalAll(sig syscall.Signal) int {
	pids := r.descendants()
	fds := map[int]int{}
	for _, pid := range pids {
		fd, errno := r.open(pid)
		switch errno {
		case 0:
			fds[pid] = fd
		case syscall.ESRCH:
			// Gone already.
		default:
			// No pidfd (ENOSYS before Linux 5.3, EPERM from a seccomp
			// filter, EMFILE): kill(2), with the race that carries,
			// rather than leave the process unsignalled.
			r.warnNoPidfd(errno)
			r.kill(pid, sig)
		}
	}
	still := map[int]bool{}
	for _, pid := range r.descendants() {
		still[pid] = true
	}
	for pid, fd := range fds {
		if still[pid] {
			if _, _, errno := syscall.Syscall6(sysPidfdSendSignal, uintptr(fd), uintptr(sig), 0, 0, 0, 0); errno != 0 && errno != syscall.ESRCH {
				r.log.Warn("could not signal a process of the turn", "pid", pid, "signal", sig, "err", errno)
			}
		}
		syscall.Close(fd)
	}
	return len(pids)
}

func (r *Reaper) kill(pid int, sig syscall.Signal) {
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		r.log.Warn("could not signal a process of the turn", "pid", pid, "signal", sig, "err", err)
	}
}

// wait waits up to d for nothing to be left below the shim.
func (r *Reaper) wait(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for len(r.descendants()) > 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}
