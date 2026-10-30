// Package hook is the logic of redoma oci-hook: the OCI hook that registers
// an annotated rootful container with redoma-xds, so the host Envoy binds
// the agent's listeners in the container's network namespace and the
// container gets the agent's SPIFFE identity, with no launcher.
//
// createRuntime (the runtime namespace, as root, after the container's
// namespaces exist and before its process runs): bind-mount the container's
// network namespace at <netns dir>/redoma-oci-<container ID>, register it,
// and wait until the control plane says Envoy's listeners are bound there.
// An error fails the container's start, and the mount is undone.
//
// poststop: unregister and unmount. Both are best effort and idempotent: a
// container whose poststop never ran is cleaned up by the control plane's
// rescan once its mount is gone.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bpalermo/redoma/common/register"
)

// State is the part of the OCI runtime state (on the hook's stdin) used here.
type State struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	Pid         int               `json:"pid"`
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations"`
}

// ErrUserns is ownNetns refusing a container with its own user namespace.
var ErrUserns = errors.New("the container has its own user namespace (--userns): Podman then attaches a bridge network after the namespace is created (postConfigureNetNS), so a network namespace without a path does not prove --network none; an agent runs in the host user namespace")

// maxSpec bounds config.json.
const maxSpec = 16 << 20

// namespace is one entry of the spec's linux.namespaces.
type namespace struct {
	Type, Path string
}

// ownNetns refuses a container whose spec does not have the runtime create a
// new network namespace for it, empty, which in rootful Podman 4.9 is
// exactly --network none in the host user namespace:
//
//   - no network namespace is the host's (--network host);
//   - one with a path is joined, not created: another container's
//     (--network container:<x>, a pod sibling) or any other
//     (--network ns:<path>), where processes that are not this agent's
//     would get its proxy and its identity; the default bridge is one too
//     (libpod creates /run/netns/netns-<uuid> and passes its path);
//   - a user namespace entry (--userns, anything but the host's) makes
//     Podman create a bridge's namespace without a path and attach the
//     network after the runtime made it (postConfigureNetNS);
//   - no PID namespace (--pid host): the agent would see the host's
//     processes, and reach through /proc/<pid>/root of one running as its
//     uid every file that process can, a SPIRE socket's directory
//     included, with no mount at all.
//
// The spec is read strictly: bundle must be an absolute path, keys match
// exactly (as the runtime's own parser does, unlike encoding/json's
// case-folding), no object repeats a key, and no namespace type appears
// twice.
func ownNetns(bundle string) error {
	raw, err := readSpec(bundle)
	if err != nil {
		return err
	}
	return specNetns(raw)
}

// specNetns is ownNetns's check of a spec already read.
func specNetns(raw []byte) error {
	nss, err := namespaces(raw)
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	return checkNamespaces(nss)
}

// readSpec reads the bundle's config.json.
func readSpec(bundle string) ([]byte, error) {
	if !filepath.IsAbs(bundle) || filepath.Clean(bundle) != bundle {
		return nil, fmt.Errorf("the bundle %q is not a clean absolute path", bundle)
	}
	f, err := os.Open(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("reading the container's spec: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSpec+1))
	if err != nil {
		return nil, fmt.Errorf("reading the container's spec: %w", err)
	}
	if len(raw) > maxSpec {
		return nil, errors.New("the container's spec is over 16 MiB")
	}
	return raw, nil
}

// checkNamespaces applies ownNetns's rule to the spec's namespaces.
func checkNamespaces(nss []namespace) error {
	var network *namespace
	seen := map[string]bool{}
	for i, ns := range nss {
		if seen[ns.Type] {
			return fmt.Errorf("the spec lists the %s namespace twice", ns.Type)
		}
		seen[ns.Type] = true
		if ns.Type == "network" {
			network = &nss[i]
		} else if ns.Path != "" && ns.Type != "user" {
			// Another process's namespace joined: its mounts, its
			// processes, its IPC are this agent's too.
			return fmt.Errorf("the container joins the %s namespace %s (--pid, --ipc, --uts or --cgroupns of another): an agent's namespaces are its own", ns.Type, ns.Path)
		}
	}
	switch {
	case seen["user"]:
		return ErrUserns
	case network == nil:
		return ErrHostNetns
	case network.Path != "":
		return fmt.Errorf("the container joins the network namespace %s (--network container:, ns:, a pod, or a bridge libpod made): an agent runs --network none", network.Path)
	case !seen[pidNamespace]:
		return ErrHostPid
	}
	return nil
}

// pidNamespace is the PID namespace's type in linux.namespaces (a value
// of the spec, not one of its keys).
const pidNamespace = "pid"

// ErrHostPid is ownNetns refusing a container in the host's PID namespace.
var ErrHostPid = errors.New("the container is in the host's PID namespace (--pid host): it would see the host's processes, and their files through /proc/<pid>/root; an agent's PID namespace is its own")

// namespaces reads linux.namespaces from a spec, with exact keys, refusing
// a repeated key anywhere in the document.
func namespaces(raw []byte) ([]namespace, error) {
	if err := noRepeatedKeys(raw); err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	var linux map[string]json.RawMessage
	if l, ok := top["linux"]; ok {
		if err := json.Unmarshal(l, &linux); err != nil {
			return nil, fmt.Errorf("linux: %w", err)
		}
	}
	var entries []map[string]json.RawMessage
	if n, ok := linux["namespaces"]; ok {
		if err := json.Unmarshal(n, &entries); err != nil {
			return nil, fmt.Errorf("linux.namespaces: %w", err)
		}
	}
	out := make([]namespace, 0, len(entries))
	for i, e := range entries {
		var ns namespace
		if err := stringField(e, "type", &ns.Type); err != nil {
			return nil, fmt.Errorf("linux.namespaces[%d]: %w", i, err)
		}
		if err := stringField(e, "path", &ns.Path); err != nil {
			return nil, fmt.Errorf("linux.namespaces[%d]: %w", i, err)
		}
		out = append(out, ns)
	}
	return out, nil
}

// stringField decodes obj[key], if present, into *v.
func stringField(obj map[string]json.RawMessage, key string, v *string) error {
	r, ok := obj[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(r, v); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// noRepeatedKeys refuses a JSON document in which an object has the same
// key twice: decoders disagree on which one wins.
func noRepeatedKeys(raw []byte) error {
	var keys keyTracker
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := keys.token(tok); err != nil {
			return err
		}
	}
}

// keyTracker follows a json.Decoder's tokens and the keys of each open
// object.
type keyTracker struct {
	stack []*jsonLevel
}

type jsonLevel struct {
	keys    map[string]bool // nil for an array
	wantKey bool
	// folded are the object's keys so far; free says the object's keys
	// are names of anything (annotations), not the spec's.
	folded  []string
	free    bool
	lastKey string
}

// specKeys are the spec's keys. runc decodes with
// encoding/json, which takes "Mounts" for "mounts"; crun does not. A key
// that is one of these but for its case, or two keys of an object that
// differ only in case, would be read differently by the two and by this
// hook: such a spec is refused.
var specKeys = strings.Fields(`ociVersion process root hostname domainname mounts hooks annotations linux
		terminal consoleSize user uid gid umask additionalGids username cwd env args commandLine
		capabilities bounding effective inheritable permitted ambient rlimits noNewPrivileges
		apparmorProfile oomScoreAdj selinuxLabel ioPriority scheduler execCPUAffinity
		path readonly destination type source options uidMappings gidMappings namespaces devices
		major minor fileMode allow access cgroupsPath resources sysctl seccomp maskedPaths readonlyPaths
		mountLabel rootfsPropagation personality intelRdt timeOffsets prestart createRuntime
		createContainer startContainer poststart poststop`)

// freeObjects are the keys whose object holds names of anything.
var freeObjects = map[string]bool{"annotations": true, "sysctl": true, "unified": true}

func (k *keyTracker) token(tok json.Token) error {
	switch tok {
	case json.Delim('{'):
		parent := k.top()
		free := parent != nil && parent.keys != nil && freeObjects[parent.lastKey]
		k.stack = append(k.stack, &jsonLevel{keys: map[string]bool{}, wantKey: true, free: free})
		return nil
	case json.Delim('['):
		k.stack = append(k.stack, &jsonLevel{})
		return nil
	case json.Delim('}'), json.Delim(']'):
		k.stack = k.stack[:len(k.stack)-1]
		k.valueEnded()
		return nil
	}
	if top := k.top(); top != nil && top.keys != nil && top.wantKey {
		key, _ := tok.(string)
		if top.keys[key] {
			return fmt.Errorf("the key %q appears twice in an object", key)
		}
		if err := top.caseOf(key); err != nil {
			return err
		}
		top.keys[key], top.wantKey, top.lastKey = true, false, key
		return nil
	}
	k.valueEnded()
	return nil
}

// caseOf refuses a key that is a spec key but for its case, or that
// differs from another key of the object only in case.
func (l *jsonLevel) caseOf(key string) error {
	if l.free {
		return nil
	}
	// strings.EqualFold folds as encoding/json's own matching does
	// (unicode.SimpleFold), so "ſource" and the Kelvin sign are caught,
	// not only the ASCII cases.
	for _, prev := range l.folded {
		if strings.EqualFold(prev, key) {
			return fmt.Errorf("the keys %q and %q differ only in case", prev, key)
		}
	}
	for _, want := range specKeys {
		if want != key && strings.EqualFold(want, key) {
			return fmt.Errorf("the key %q is the spec's %q in another case (runc would read it, crun would not)", key, want)
		}
	}
	l.folded = append(l.folded, key)
	return nil
}

func (k *keyTracker) top() *jsonLevel {
	if len(k.stack) == 0 {
		return nil
	}
	return k.stack[len(k.stack)-1]
}

// valueEnded: in an object, a key comes next.
func (k *keyTracker) valueEnded() {
	if top := k.top(); top != nil && top.keys != nil {
		top.wantKey = true
	}
}

// Mounter mounts a process's network namespace at a path, and unmounts it.
type Mounter interface {
	// BindNetns bind-mounts /proc/<pid>/ns/net at target, replacing a stale
	// mount there.
	BindNetns(pid int, target string) error
	// Unmount removes the mount at target and target itself; nothing there
	// is not an error.
	Unmount(target string) error
}

// Hook holds the hook's settings.
type Hook struct {
	// Annotation names the agent: <Annotation>=<agent name>.
	Annotation string
	// NetnsDir is where namespaces are mounted (/run/netns): the host Envoy
	// sees mounts made there.
	NetnsDir string
	// Timeout bounds the wait for the control plane at createRuntime.
	Timeout time.Duration
	Mount   Mounter
	// BindProcess opens a pidfd for the container's process, the one whose
	// network namespace is mounted at netns; nil is BindProcess. An error,
	// or no file (a test's), registers the container without one.
	BindProcess func(pid int, netns, container string) (*os.File, error)
	// Dial connects to the control plane's registration socket.
	Dial func(ctx context.Context) (net.Conn, error)
	// Log takes the hook's messages (the runtime keeps its stderr).
	Log io.Writer
	// ServiceIDs are the uids and gids an agent's process may not have;
	// nil is ServiceIDs (the package's).
	ServiceIDs map[uint32]string
	// Mountinfo reads a process's mounts ("self": the hook's, the host's);
	// nil reads /proc. Tests set their own.
	Mountinfo func(pid string) ([]mountEntry, error)
	// Forbidden and Graphroot are ForbiddenSources and Graphroot unless a
	// test sets a tree of its own.
	Forbidden []string
	Graphroot string
	// Runroot is Runroot unless set.
	Runroot string
}

func (h *Hook) logf(format string, args ...any) {
	fmt.Fprintf(h.Log, "redoma oci-hook: "+format+"\n", args...)
}

// CreateRuntime registers the container. A container without the
// annotation is not an agent: nothing happens.
func (h *Hook) CreateRuntime(ctx context.Context, st State) error {
	name, ok := st.Annotations[h.Annotation]
	if !ok {
		return nil
	}
	if !register.ValidContainer(st.ID) {
		return fmt.Errorf("not a container ID: %q", st.ID)
	}
	if st.Pid <= 0 {
		return fmt.Errorf("container %s: no pid in the state", st.ID)
	}
	// Its own network namespace, new: not the host's (its listeners would be
	// on the host's loopback, for any local process to use), not one it
	// shares. The spec says what the runtime made; BindNetns checks the host
	// case again on the namespace it mounts.
	// Nor a way to the host's services: their identities, root, gained
	// privileges, capabilities, their sockets mounted (checkSpec).
	raw, err := readSpec(st.Bundle)
	if err != nil {
		return fmt.Errorf("container %s: %w", st.ID, err)
	}
	if err := specNetns(raw); err != nil {
		return fmt.Errorf("container %s: %w", st.ID, err)
	}
	if err := h.checkSpec(raw, st); err != nil {
		return fmt.Errorf("container %s: %w", st.ID, err)
	}
	target := register.NetnsPath(h.NetnsDir, st.ID)
	if err := h.Mount.BindNetns(st.Pid, target); err != nil {
		return fmt.Errorf("mounting the network namespace of container %s: %w", st.ID, err)
	}
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	// A PID found in another container's cgroup was reused, maybe before the
	// mount, which may then be of another container's namespace: the start
	// fails. A process that cannot be bound otherwise is registered without
	// its pidfd: in Delegated mode nothing needs one, and in Broker mode
	// redoma xds then serves the agent no SVID (it fails closed). The start
	// does not depend on a check (the container's own cgroup) only a host
	// can confirm.
	proc, err := h.bindProcess()(st.Pid, target, st.ID)
	switch {
	case errors.Is(err, ErrPIDReused):
		err = fmt.Errorf("the container's process: %w", err)
	case err != nil:
		h.logf("container %s: its process cannot be bound to a pidfd (%v); registering it without one, so in Broker mode agent %s gets no SVID", st.ID, err, name)
		proc = nil
		err = nil
	}
	if err == nil {
		err = h.call(ctx, register.Request{Op: register.OpRegister, Container: st.ID, Agent: name, Netns: target, Pidfd: proc != nil}, proc)
	}
	if proc != nil {
		proc.Close()
	}
	if err == nil {
		h.logf("container %s is agent %s, its listeners bound in %s", st.ID, name, target)
		return nil
	}
	// Undo: the start fails, and nothing of the claim may stay.
	undo, cancelUndo := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelUndo()
	if uerr := h.call(undo, register.Request{Op: register.OpUnregister, Container: st.ID}, nil); uerr != nil {
		h.logf("container %s: unregistering after a failed register: %v", st.ID, uerr)
	}
	if merr := h.Mount.Unmount(target); merr != nil {
		h.logf("container %s: unmounting %s after a failed register: %v", st.ID, target, merr)
	}
	return fmt.Errorf("registering container %s as agent %s: %w", st.ID, name, err)
}

// Poststop unregisters the container and unmounts its namespace. A
// control plane that cannot be reached is logged, not an error: the rescan
// removes the registration once the mount is gone.
func (h *Hook) Poststop(ctx context.Context, st State) error {
	if _, ok := st.Annotations[h.Annotation]; !ok {
		return nil
	}
	if !register.ValidContainer(st.ID) {
		return fmt.Errorf("not a container ID: %q", st.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.call(ctx, register.Request{Op: register.OpUnregister, Container: st.ID}, nil); err != nil {
		h.logf("container %s: unregistering: %v (the control plane's rescan will)", st.ID, err)
	}
	return h.Mount.Unmount(register.NetnsPath(h.NetnsDir, st.ID))
}

func (h *Hook) bindProcess() func(pid int, netns, container string) (*os.File, error) {
	if h.BindProcess != nil {
		return h.BindProcess
	}
	return BindProcess
}

// call sends one request, with proc if it is not nil, and returns the
// control plane's error, if any.
func (h *Hook) call(ctx context.Context, req register.Request, proc *os.File) error {
	conn, err := h.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// The control plane may answer and close before reading the request (it
	// refuses a peer that is not root first): the write then fails, and the
	// answer is still there to read.
	werr := register.WriteRequest(conn, req, proc)
	var resp register.Response
	if err := register.Read(conn, &resp); err != nil {
		if werr != nil {
			return werr
		}
		if errors.Is(err, io.EOF) {
			return errors.New("the control plane closed the connection without an answer")
		}
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}
