// Package hostcheck is what redoma xds checks of the host before it uses
// SPIRE's Broker API (docs/broker-identity.md, "The mitigation"): with the
// agents' entries selecting on an attestable container label, any process
// of an agent that reached SPIRE's Workload API could obtain the agent's
// SVID itself, so the sockets must be where the OCI hook keeps every agent
// from them, and the Workload API's directory closed to everyone else.
package hostcheck

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/bpalermo/redoma/common/forbidden"
)

// SocketsForbidden refuses a socket whose directory, resolved through
// symlinks, is not under sources: the paths the OCI hook forbids every
// agent to have mounted (forbidden.Sources of the settings file, which the
// hook reads; a flag of redoma xds alone could point elsewhere).
func SocketsForbidden(sockets, sources []string) error {
	for _, socket := range sockets {
		dir, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(socket)))
		if err != nil {
			return fmt.Errorf("SPIRE's socket %s: its directory: %w", socket, err)
		}
		if !forbidden.Under(dir, sources) {
			return fmt.Errorf("SPIRE's socket %s is in %s, which the OCI hook does not keep from the agents: set it in /etc/redoma/redoma.conf, which the hook reads, not only on redoma xds's command line", socket, dir)
		}
	}
	return nil
}

// Accounts are the files the users and groups are read from.
type Accounts struct {
	Passwd, Group string
}

// System is the host's /etc/passwd and /etc/group.
var System = Accounts{Passwd: "/etc/passwd", Group: "/etc/group"}

// WorkloadDir refuses a Workload API socket directory that grants any
// permission to others, that its group may write, or whose group has a
// member (named in the group file, or a user whose primary group it is)
// other than root and the uids in allowed. SPIRE makes the socket itself
// 0777, so the directory is what keeps other users from it. Users and
// groups from elsewhere than the files (NSS, a directory service) are not
// seen.
func WorkloadDir(dir string, allowed []uint32, acc Accounts) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("the Workload API's directory: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() {
		return fmt.Errorf("the Workload API's directory %s is not a directory", dir)
	}
	switch perm := fi.Mode().Perm(); {
	case perm&0o007 != 0:
		return fmt.Errorf("the Workload API's directory %s is %04o: it grants others access; make it 0750 or tighter", dir, perm)
	case perm&0o020 != 0:
		return fmt.Errorf("the Workload API's directory %s is %04o: its group may write it", dir, perm)
	}
	members, err := groupMembers(st.Gid, acc)
	if err != nil {
		return err
	}
	for name, uid := range members {
		if uid != 0 && !slices.Contains(allowed, uid) {
			return fmt.Errorf("the Workload API's directory %s has group %d, of which %s (uid %d) is a member: only %v may be", dir, st.Gid, name, uid, allowed)
		}
	}
	return nil
}

// groupMembers are the users of gid, by name, with their uids: those the
// group file lists, and those whose primary group it is.
func groupMembers(gid uint32, acc Accounts) (map[string]uint32, error) {
	users := map[string]uint32{}
	primary := map[string]uint32{}
	err := readColon(acc.Passwd, 7, func(f []string) error {
		uid, err1 := strconv.ParseUint(f[2], 10, 32)
		g, err2 := strconv.ParseUint(f[3], 10, 32)
		if err := errors.Join(err1, err2); err != nil {
			return err
		}
		users[f[0]] = uint32(uid)
		if uint32(g) == gid {
			primary[f[0]] = uint32(uid)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = readColon(acc.Group, 4, func(f []string) error {
		g, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil || uint32(g) != gid || f[3] == "" {
			return err
		}
		for _, name := range strings.Split(f[3], ",") {
			uid, ok := users[name]
			if !ok {
				return fmt.Errorf("group %d lists %s, who is not in %s", gid, name, acc.Passwd)
			}
			primary[name] = uid
		}
		return nil
	})
	return primary, err
}

// readColon calls line with the fields of each line of a colon-separated
// file of n fields, skipping blank lines and comments.
func readColon(path string, n int, line func([]string) error) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, ":")
		if len(fields) != n {
			return fmt.Errorf("%s: a line of %d fields, not %d", path, len(fields), n)
		}
		if err := line(fields); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return sc.Err()
}
