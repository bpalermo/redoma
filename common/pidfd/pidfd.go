// Package pidfd holds a process by a pidfd (pidfd_open(2)): a descriptor
// that names one process, not a number the kernel may give to another once
// it exits.
//
// The OCI hook opens one for an agent's container process and hands it to
// redoma xds, which names the process to SPIRE's Broker API by its PID and
// checks, after SPIRE has attested it, that the process the descriptor
// holds is still alive: a live process keeps its PID, so the PID SPIRE read
// was its.
package pidfd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// ErrExited is a process that has exited (a zombie included) or was reaped.
var ErrExited = errors.New("the process has exited")

// Open opens a pidfd for pid, close-on-exec.
func Open(pid int) (*os.File, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open(%d): %w", pid, err)
	}
	return os.NewFile(uintptr(fd), "pidfd:"+strconv.Itoa(pid)), nil
}

// FD is a pidfd. Its methods are safe from any goroutine, during and after
// Close: a closed FD is an exited process, never another descriptor that
// took its number.
type FD struct {
	f *os.File
}

// New takes f, which must be a pidfd (its fdinfo has a Pid: line). It owns
// f from then on, and closes it if it is refused.
func New(f *os.File) (*FD, error) {
	p := &FD{f: f}
	if _, err := p.info(); err != nil {
		f.Close()
		return nil, err
	}
	return p, nil
}

// File is the descriptor, to pass on (SCM_RIGHTS, systemd's store). It
// stays the FD's.
func (p *FD) File() *os.File { return p.f }

// Close closes the descriptor.
func (p *FD) Close() error { return p.f.Close() }

// PID is the process's PID in this process's PID namespace, from the
// pidfd's fdinfo: ErrExited once it was reaped.
func (p *FD) PID() (int, error) {
	pid, err := p.info()
	if err != nil {
		return 0, err
	}
	switch {
	case pid == -1:
		return 0, ErrExited
	case pid == 0:
		return 0, errors.New("the process is in a PID namespace this one cannot see")
	}
	return pid, nil
}

// Alive reports whether the process has not exited: poll(2) on a pidfd
// reports POLLIN once it has, zombie or reaped.
func (p *FD) Alive() bool {
	alive := false
	err := p.control(func(fd int) error {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fds, 0)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return err
			}
			alive = n == 0
			return nil
		}
	})
	return err == nil && alive
}

// info reads the Pid: line of the descriptor's fdinfo, which only a pidfd
// has: the PID, or -1 once the process was reaped.
func (p *FD) info() (int, error) {
	var pid int
	err := p.control(func(fd int) error {
		raw, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
		if err != nil {
			return err
		}
		pid, err = parsePid(raw)
		return err
	})
	return pid, err
}

// control runs fn on the descriptor, refusing a closed FD.
func (p *FD) control(fn func(fd int) error) error {
	raw, err := p.f.SyscallConn()
	if err != nil {
		return err
	}
	var fnErr error
	if err := raw.Control(func(fd uintptr) { fnErr = fn(int(fd)) }); err != nil {
		return err
	}
	return fnErr
}

// parsePid reads the Pid: line of a pidfd's fdinfo.
func parsePid(fdinfo []byte) (int, error) {
	sc := bufio.NewScanner(bytes.NewReader(fdinfo))
	for sc.Scan() {
		value, ok := bytes.CutPrefix(sc.Bytes(), []byte("Pid:"))
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(string(bytes.TrimSpace(value)))
		if err != nil {
			return 0, fmt.Errorf("a pidfd's fdinfo: Pid: %w", err)
		}
		return pid, nil
	}
	return 0, errors.New("not a pidfd: its fdinfo has no Pid: line")
}
