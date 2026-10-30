package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/bpalermo/redoma/common/pidfd"
)

// BindProcess opens a pidfd for the container's process, pid, at
// createRuntime, while the runtime holds it, and makes sure it is that
// container's process and not one that took its PID:
//
//   - its network namespace is the one the hook mounted at netns (the
//     same device and inode): a process that took the PID between the
//     mount and pidfd_open is in another namespace;
//   - its cgroup is the container's, libpod-<container>: a process that
//     took the PID before the mount (the held process killed by root or
//     the OOM killer), whose namespace the hook then mounted, is in
//     another cgroup;
//   - the pidfd still reports it alive, so the /proc files read above were
//     the pidfd's process's.
//
// redoma xds names the process to SPIRE by this pidfd's PID and checks it
// is still alive after each attestation. A process in another container's
// cgroup is ErrPIDReused, which fails the start; one that fails the checks
// otherwise is registered without a pidfd (CreateRuntime): in Broker mode
// its agent then gets no SVID.
func BindProcess(pid int, netns, container string) (*os.File, error) {
	return processBinder{proc: "/proc"}.bind(pid, netns, container)
}

// processBinder is BindProcess with /proc where a test says.
type processBinder struct {
	proc string
}

func (b processBinder) bind(pid int, netns, container string) (*os.File, error) {
	f, err := pidfd.Open(pid)
	if err != nil {
		return nil, err
	}
	p, err := pidfd.New(f)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(b.proc, strconv.Itoa(pid))
	if err := b.check(p, dir, netns, container); err != nil {
		p.Close()
		return nil, err
	}
	return p.File(), nil
}

func (b processBinder) check(p *pidfd.FD, dir, netns, container string) error {
	same, err := samePath(netns, filepath.Join(dir, "ns", "net"))
	if err != nil {
		return err
	}
	if !same {
		return errors.New("its network namespace is not the one mounted")
	}
	cgroup, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		return err
	}
	if other := otherContainer(string(cgroup), container); other != "" {
		return fmt.Errorf("%w: it is in container %s's cgroup, not container %s's", ErrPIDReused, other, container)
	}
	if !inContainerCgroup(string(cgroup), container) {
		return fmt.Errorf("its cgroup is not container %s's (libpod-%s) yet", container, container)
	}
	if !p.Alive() {
		return errors.New("it exited while the hook ran")
	}
	return nil
}

// ErrPIDReused is BindProcess finding the PID in another container's cgroup
// (libpod-<another ID>): the process the runtime held was replaced, maybe
// before the hook mounted its namespace, which may then be another
// container's. CreateRuntime fails the start on it; any other failure to
// bind only registers the container without a pidfd.
var ErrPIDReused = errors.New("the container's PID was reused")

// libpodScope is a container's cgroup path element: libpod-<ID>, with
// .scope (Podman's systemd cgroup manager) or without (cgroupfs).
var libpodScope = regexp.MustCompile(`^libpod-([0-9a-f]{64})(\.scope)?$`)

// otherContainer is the ID of another container than container whose
// cgroup a /proc/<pid>/cgroup names, or "".
func otherContainer(cgroup, container string) string {
	for line := range strings.Lines(cgroup) {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		for el := range strings.SplitSeq(parts[2], "/") {
			if m := libpodScope.FindStringSubmatch(el); m != nil && m[1] != container {
				return m[1]
			}
		}
	}
	return ""
}

// samePath reports whether a and b are the same file (device and inode).
func samePath(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("stat %s: %w", a, err)
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("stat %s: %w", b, err)
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino, nil
}

// inContainerCgroup reports whether a /proc/<pid>/cgroup names container's
// cgroup on every line with a path: a path element libpod-<container>,
// with .scope (Podman's systemd cgroup manager) or without (cgroupfs).
func inContainerCgroup(cgroup, container string) bool {
	found := false
	for line := range strings.Lines(cgroup) {
		// hierarchy-ID:controllers:path
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || parts[2] == "" || parts[2] == "/" {
			continue
		}
		if !hasContainerElement(parts[2], container) {
			return false
		}
		found = true
	}
	return found
}

func hasContainerElement(path, container string) bool {
	for el := range strings.SplitSeq(path, "/") {
		if el == "libpod-"+container || el == "libpod-"+container+".scope" {
			return true
		}
	}
	return false
}
