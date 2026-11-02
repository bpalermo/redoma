package hook

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bpalermo/redoma/common/register"
)

// MCP upstreams (docs/mcp-upstreams.md). A rootful container annotated
// redoma/upstream=<name> is a host MCP upstream's server: not an agent, no
// identity, in a user namespace of its own (--userns=auto), with its
// credential's directory mounted and nothing else of the host's.
//
// createRuntime: the spec's checks (its own user namespace mapped into the
// containers subordinate ranges, its network and PID namespaces new, no
// privilege, the mounts an agent may have plus its own credential
// directory); then the namespace is mounted at
// <netns dir>/redoma-mcp-<container ID>. Nothing is registered yet: with a
// user namespace, Podman attaches a network after the runtime made the
// namespace, so createRuntime cannot tell --network none.
//
// poststart: the namespace must have no interface but lo. Then the
// upstream is registered, and redoma xds publishes its egress listener and
// cluster. Otherwise it is unmounted and never registered: whatever network
// it has, Envoy never sends it a request. A poststart error is the
// runtime's warning, not a failed start: the container then runs, alone.

// UpstreamTokenRoot is where the upstreams' credentials live on the host
// (mcpconf.TokenDir): an upstream may mount its own directory under it,
// <root>/<name>, and nothing else of it; an agent nothing of it.
const UpstreamTokenRoot = "/run/redoma-mcp"

// Subordinate ranges, from /etc/subuid and /etc/subgid.
const (
	SubUID = "/etc/subuid"
	SubGID = "/etc/subgid"
	// SubordinateUser is the user whose ranges rootful Podman allocates
	// --userns=auto from.
	SubordinateUser = "containers"
)

// idRange is a range of host ids, [start, start+size).
type idRange struct {
	start, size uint64
}

func (r idRange) holds(id uint64) bool {
	return id >= r.start && id-r.start < r.size
}

// inRanges reports whether [start, start+size) lies inside one of ranges.
func inRanges(ranges []idRange, start, size uint64) bool {
	for _, r := range ranges {
		if start >= r.start && size <= r.size && start-r.start <= r.size-size {
			return true
		}
	}
	return false
}

// readSubordinate returns user's ranges in a subuid or subgid file. A
// missing file has none.
func readSubordinate(path, user string) ([]idRange, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []idRange
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(parts) != 3 || parts[0] != user {
			continue
		}
		start, err1 := strconv.ParseUint(parts[1], 10, 32)
		size, err2 := strconv.ParseUint(parts[2], 10, 32)
		if err1 != nil || err2 != nil || size == 0 {
			return nil, fmt.Errorf("%s: a %s line that is not <start>:<count>: %q", path, user, sc.Text())
		}
		out = append(out, idRange{start: start, size: size})
	}
	return out, sc.Err()
}

// subordinate are the containers ranges, uids then gids.
func (h *Hook) subordinate() (uids, gids []idRange, err error) {
	uidFile, gidFile := h.SubUID, h.SubGID
	if uidFile == "" {
		uidFile = SubUID
	}
	if gidFile == "" {
		gidFile = SubGID
	}
	if uids, err = readSubordinate(uidFile, SubordinateUser); err != nil {
		return nil, nil, err
	}
	if gids, err = readSubordinate(gidFile, SubordinateUser); err != nil {
		return nil, nil, err
	}
	return uids, gids, nil
}

// checkNotSubordinate refuses an agent whose uid, gid or any additional
// gid is in the containers ranges: an upstream's process holds those, and
// no agent shares an id with one.
func (h *Hook) checkNotSubordinate(top map[string]json.RawMessage) error {
	uids, gids, err := h.subordinate()
	if err != nil {
		return fmt.Errorf("the subordinate ids: %w", err)
	}
	p, err := specProcess(top)
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	for _, r := range uids {
		if r.holds(uint64(p.uid)) {
			return fmt.Errorf("the container runs as uid %d, in the %s subordinate range that MCP upstreams run in: an agent never shares an id with one", p.uid, SubordinateUser)
		}
	}
	for _, g := range append([]uint32{p.gid}, p.additional...) {
		for _, r := range gids {
			if r.holds(uint64(g)) {
				return fmt.Errorf("the container has gid %d, in the %s subordinate range that MCP upstreams run in: an agent never shares an id with one", g, SubordinateUser)
			}
		}
	}
	return nil
}

// networkNamespace is the network namespace's type in linux.namespaces.
const networkNamespace = "network"

// checkUpstreamNamespaces is the rule of an upstream's namespaces: a user
// namespace of its own (the runtime creates it from the spec's mappings),
// network and PID namespaces created for it, and no other namespace joined.
func checkUpstreamNamespaces(nss []namespace) error {
	seen := map[string]*namespace{}
	for i, ns := range nss {
		if seen[ns.Type] != nil {
			return fmt.Errorf("the spec lists the %s namespace twice", ns.Type)
		}
		seen[ns.Type] = &nss[i]
		if ns.Path != "" {
			return fmt.Errorf("the container joins the %s namespace %s: an MCP upstream's namespaces are its own", ns.Type, ns.Path)
		}
	}
	switch {
	case seen["user"] == nil:
		return errors.New("the container has no user namespace of its own: an MCP upstream runs --userns=auto")
	case seen[networkNamespace] == nil:
		return ErrHostNetns
	case seen[pidNamespace] == nil:
		return ErrHostPid
	}
	return nil
}

// mapping is one entry of linux.uidMappings or gidMappings.
type mapping struct {
	ContainerID uint64 `json:"containerID"`
	HostID      uint64 `json:"hostID"`
	Size        uint64 `json:"size"`
}

// checkMappings holds the user namespace's mappings wholly inside the
// containers ranges, which hold no host id in use (root's, redoma's
// services', any agent's): see docs/mcp-upstreams.md, "Users".
func (h *Hook) checkMappings(top map[string]json.RawMessage) error {
	uids, gids, err := h.subordinate()
	if err != nil {
		return fmt.Errorf("the subordinate ids: %w", err)
	}
	linux, err := object(top["linux"], "linux")
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	for _, kind := range []struct {
		key    string
		ranges []idRange
		file   string
	}{{"uidMappings", uids, SubUID}, {"gidMappings", gids, SubGID}} {
		var ms []mapping
		if err := field(linux, kind.key, "linux", &ms); err != nil {
			return fmt.Errorf("the container's spec: %w", err)
		}
		if len(ms) == 0 {
			return fmt.Errorf("the container's user namespace has no %s: an MCP upstream runs --userns=auto", kind.key)
		}
		for _, m := range ms {
			if m.Size == 0 || !inRanges(kind.ranges, m.HostID, m.Size) {
				return fmt.Errorf("the container maps host ids %d-%d (%s), outside the %s ranges of %s: an MCP upstream runs --userns=auto", m.HostID, m.HostID+m.Size-1, kind.key, SubordinateUser, kind.file)
			}
		}
	}
	return nil
}

// checkUpstreamProcess holds an upstream's process to no privilege: no
// gaining any, and no capability past AllowedCaps. Its uid and gid are its
// user namespace's, mapped into the subordinate ranges (checkMappings).
func checkUpstreamProcess(top map[string]json.RawMessage) error {
	p, err := specProcess(top)
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	if !p.noNewPrivileges {
		return errors.New("the container may gain privileges: an MCP upstream runs with --security-opt no-new-privileges")
	}
	return checkCaps(p)
}

// tokenRoot is UpstreamTokenRoot unless a test sets its own.
func (h *Hook) tokenRoot() string {
	if h.TokenRoot != "" {
		return h.TokenRoot
	}
	return UpstreamTokenRoot
}

// upstreamForbidden are the sources an upstream may not mount: an agent's,
// and every other upstream's credential directory. Its own, <root>/<name>,
// is the one host directory it may have.
func (h *Hook) upstreamForbidden() []string {
	forbidden, _, _ := h.storage()
	return append(slicesClone(forbidden), h.tokenRoot())
}

func slicesClone(s []string) []string {
	return append([]string(nil), s...)
}

// createRuntimeUpstream checks an upstream's container and mounts its
// namespace; poststart registers it.
func (h *Hook) createRuntimeUpstream(st State, name string) error {
	if !register.ValidContainer(st.ID) {
		return fmt.Errorf("not a container ID: %q", st.ID)
	}
	if st.Pid <= 0 {
		return fmt.Errorf("container %s: no pid in the state", st.ID)
	}
	raw, err := readSpec(st.Bundle)
	if err != nil {
		return fmt.Errorf("container %s: %w", st.ID, err)
	}
	if err := h.checkUpstreamSpec(raw, st, name); err != nil {
		return fmt.Errorf("container %s, MCP upstream %s: %w", st.ID, name, err)
	}
	target := register.UpstreamNetnsPath(h.NetnsDir, st.ID)
	if err := h.Mount.BindNetns(st.Pid, target); err != nil {
		return fmt.Errorf("mounting the network namespace of container %s: %w", st.ID, err)
	}
	h.logf("container %s is MCP upstream %s, its namespace at %s; registered at poststart", st.ID, name, target)
	return nil
}

// checkUpstreamSpec is createRuntime's check of an upstream's spec.
func (h *Hook) checkUpstreamSpec(raw []byte, st State, name string) error {
	nss, err := namespaces(raw)
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	if err := checkUpstreamNamespaces(nss); err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	if err := h.checkMappings(top); err != nil {
		return err
	}
	if err := checkUpstreamProcess(top); err != nil {
		return err
	}
	return h.checkMountsOf(top, st, h.upstreamForbidden(), filepath.Join(h.tokenRoot(), name))
}

// Interfaces lists the interfaces of the network namespace mounted at
// netns, entering it on a thread of its own, which then exits.
func Interfaces(netns string) ([]string, error) {
	type result struct {
		names []string
		err   error
	}
	out := make(chan result, 1)
	go func() {
		// Never unlocked: the thread left in another namespace dies with
		// the goroutine.
		runtime.LockOSThread()
		f, err := os.Open(netns)
		if err != nil {
			out <- result{err: err}
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			out <- result{err: fmt.Errorf("entering %s: %w", netns, err)}
			return
		}
		ifs, err := net.Interfaces()
		var names []string
		for _, i := range ifs {
			names = append(names, i.Name)
		}
		out <- result{names: names, err: err}
	}()
	r := <-out
	return r.names, r.err
}

// Poststart registers an upstream's container, once its namespace has no
// interface but lo. A container that is no upstream is not this stage's.
func (h *Hook) Poststart(ctx context.Context, st State) error {
	name, ok := st.Annotations[h.UpstreamAnnotation]
	if !ok || h.UpstreamAnnotation == "" {
		return nil
	}
	if !register.ValidContainer(st.ID) {
		return fmt.Errorf("not a container ID: %q", st.ID)
	}
	target := register.UpstreamNetnsPath(h.NetnsDir, st.ID)
	list := h.Interfaces
	if list == nil {
		list = Interfaces
	}
	names, err := list(target)
	if err == nil && (len(names) != 1 || names[0] != "lo") {
		err = fmt.Errorf("its network namespace has the interfaces %q: an MCP upstream runs --network none, with lo alone", names)
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, h.Timeout)
		defer cancel()
		err = h.call(ctx, register.Request{Op: register.OpRegister, Container: st.ID, Upstream: name, Netns: target}, nil)
	}
	if err != nil {
		if merr := h.Mount.Unmount(target); merr != nil {
			h.logf("container %s: unmounting %s: %v", st.ID, target, merr)
		}
		return fmt.Errorf("MCP upstream %s (container %s) is not registered, and gets no request: %w", name, st.ID, err)
	}
	h.logf("container %s is MCP upstream %s, registered", st.ID, name)
	return nil
}

// poststopUpstream unregisters an upstream's container and unmounts its
// namespace, as Poststop does an agent's.
func (h *Hook) poststopUpstream(ctx context.Context, st State) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.call(ctx, register.Request{Op: register.OpUnregister, Container: st.ID}, nil); err != nil {
		h.logf("container %s: unregistering: %v (the control plane's rescan will)", st.ID, err)
	}
	return h.Mount.Unmount(register.UpstreamNetnsPath(h.NetnsDir, st.ID))
}
