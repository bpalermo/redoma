// Package hooksource is the agent.Source of the Podman OCI hook
// (redoma oci-hook): a rootful container annotated
// redoma/agent=<name> is registered by the hook, which runs as
// root in the runtime's createRuntime stage, and unregistered in poststop.
//
// The name policy is the security boundary. An agent's SPIFFE identity
// (spiffe://<td>/agent/<name>, selected by redoma:agent:<name>) follows from
// its NAME alone, so who may claim which name decides who gets which
// identity:
//
//   - who: root only. Registrations arrive on a unix socket
//     (common/register), 0600 and owned by redoma-sds, and the peer's uid,
//     read from SO_PEERCRED, must be 0. Rootless Podman, whose hooks run as
//     the user, can never claim a name.
//   - which names: those in the allowlist, /etc/redoma/agents.txt, read on
//     every register: a name is allowed when a line of the file is exactly
//     the name (as `grep -qxF` reads it), so no
//     comment, no surrounding space. The file, and every directory above it,
//     must be owned by root (AllowlistOwner) and writable by no one else,
//     and the file must not be a symlink, or no name is allowed: whoever can
//     change it can mint identities.
//   - one holder: one live container holds a name, and a namespace, at a
//     time, and a name a launcher runs is the launcher's (agent.Merge).
//
// The hook refuses a container in the host's network namespace, and the
// control plane one in another agent's.
//
// A register is answered once Envoy has bound the agent's listeners
// (agent.Event.Bound), so the hook can hold the container's start until its
// proxy is there. Registrations are recorded in StateFile, so a restart of
// redoma-xds finds them again, and rescanned: one whose namespace is gone
// (unmounted by poststop, or by hand after a crash) is removed.
package hooksource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/bpalermo/redoma/common/allowlist"
	"github.com/bpalermo/redoma/common/atomicfile"
	"github.com/bpalermo/redoma/common/register"
	"github.com/bpalermo/redoma/xds/internal/activedir"
	"github.com/bpalermo/redoma/xds/internal/agent"
)

// Selectors are what a hook-registered agent's identity is subscribed by:
// redoma:agent:<name>, the registrar-neutral selector the SPIRE entries
// select on, also sent by activedir. The identity is therefore the NAME: whoever may claim a name
// gets spiffe://<td>/agent/<name>. That is decided by who may register
// (root, through the socket) and which names (the allowlist): see Source.
func Selectors(name string) []agent.Selector {
	return []agent.Selector{{Type: "redoma", Value: "agent:" + name}}
}

// Source serves the registration socket. Set the fields, then Run.
type Source struct {
	// Listener is the registration socket (Listen).
	Listener net.Listener
	// Allowlist is the file of names a container may claim
	// (/etc/redoma/agents.txt): a name is allowed when a line is exactly
	// the name, as `grep -qxF` reads it. Any other line
	// (a comment, an indented name) allows nothing; a line with a carriage
	// return makes the file refused whole (see allowed).
	Allowlist string
	// AllowlistOwner is the uid that must own Allowlist and the
	// directories above it: 0, root, unless a test sets its own.
	AllowlistOwner uint32
	// AllowlistRoot is the highest directory checked above Allowlist: "/"
	// unless a test sets the temporary directory it works in.
	AllowlistRoot string
	// NetnsDir is where the hook mounts the namespaces (/run/netns).
	NetnsDir string
	// StateFile records the registrations across restarts.
	StateFile string
	// Interval is how often the registered namespaces are checked.
	Interval time.Duration
	// BoundTimeout bounds how long a register waits for Envoy.
	BoundTimeout time.Duration
	Log          *slog.Logger

	// PeerUID and Stat are the system's unless a test replaces them.
	PeerUID func(net.Conn) (uint32, error)
	Stat    activedir.StatFunc

	mu   sync.Mutex
	regs map[string]registration // by container ID
	emit func(agent.Event)
}

var _ agent.Source = (*Source)(nil)

// registration is one container's claim, as recorded in StateFile.
type registration struct {
	Agent   string        `json:"agent"`
	Netns   string        `json:"netns"`
	NetnsID agent.NetnsID `json:"netns_id"`
}

func (r registration) agent() agent.Agent {
	return agent.Agent{ID: r.Agent, Netns: r.Netns, NetnsID: r.NetnsID, Selectors: Selectors(r.Agent)}
}

// Listen opens the registration socket at path, replacing a stale one, with
// mode 0600: its owner, and root.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// PeerUID is the uid of the process at the other end of a unix socket.
func PeerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix socket: %T", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

// Run implements agent.Source: the recorded registrations whose namespace
// is still there, Synced, then each registration and its end.
func (s *Source) Run(ctx context.Context, emit func(agent.Event)) error {
	if s.PeerUID == nil {
		s.PeerUID = PeerUID
	}
	if s.Stat == nil {
		s.Stat = activedir.StatNetns
	}
	s.mu.Lock()
	s.emit = emit
	s.regs = s.load()
	for _, r := range s.regs {
		emit(agent.Event{Agent: r.agent()})
	}
	s.save()
	s.mu.Unlock()
	emit(agent.Event{Synced: true})

	go func() {
		<-ctx.Done()
		s.Listener.Close()
	}()
	go s.rescan(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := s.Listener.Accept()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("registration socket: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

// load reads StateFile, keeping the registrations whose namespace is still
// the recorded one.
func (s *Source) load() map[string]registration {
	regs := map[string]registration{}
	raw, err := os.ReadFile(s.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return regs
	}
	var recorded map[string]registration
	if err == nil {
		err = json.Unmarshal(raw, &recorded)
	}
	if err != nil {
		s.Log.Error("cannot read the hook registrations; their containers need a restart to be registered again", "file", s.StateFile, "err", err)
		return regs
	}
	for container, r := range recorded {
		if s.live(r) {
			regs[container] = r
		}
	}
	return regs
}

// save records the registrations; called with mu held.
func (s *Source) save() {
	raw, err := json.Marshal(s.regs)
	if err == nil {
		err = atomicfile.Write(s.StateFile, raw, 0o600)
	}
	if err != nil {
		s.Log.Error("cannot record the hook registrations", "file", s.StateFile, "err", err)
	}
}

// live reports whether r's namespace is still the one registered.
func (s *Source) live(r registration) bool {
	id, err := s.Stat(r.Netns)
	return err == nil && id == r.NetnsID
}

// rescan removes, every Interval, the registrations whose namespace is gone
// or another: the poststop hook unmounted it, or someone did after a crash.
func (s *Source) rescan(ctx context.Context) {
	tick := time.NewTicker(s.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.mu.Lock()
		for container, r := range s.regs {
			if !s.live(r) {
				s.Log.Info("hook registration's namespace is gone; removing it", "container", container, "agent", r.Agent)
				s.drop(container)
			}
		}
		s.mu.Unlock()
	}
}

// drop ends container's registration; called with mu held.
func (s *Source) drop(container string) {
	r, ok := s.regs[container]
	if !ok {
		return
	}
	delete(s.regs, container)
	s.save()
	s.emit(agent.Event{Agent: r.agent(), Removed: true})
}

func (s *Source) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(s.BoundTimeout + 10*time.Second))
	err := s.serve(ctx, conn)
	resp := register.Response{}
	if err != nil {
		resp.Error = err.Error()
	}
	if werr := register.Write(conn, resp); werr != nil {
		s.Log.Warn("could not answer the hook", "err", werr)
	}
}

func (s *Source) serve(ctx context.Context, conn net.Conn) error {
	uid, err := s.PeerUID(conn)
	if err != nil {
		return fmt.Errorf("cannot tell who is registering: %w", err)
	}
	if uid != 0 {
		s.Log.Error("refused a registration from a process that is not root", "uid", uid)
		return errors.New("only root may register agents")
	}
	var req register.Request
	if err := register.Read(conn, &req); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	if !register.ValidContainer(req.Container) {
		return fmt.Errorf("not a container ID: %q", req.Container)
	}
	switch req.Op {
	case register.OpRegister:
		return s.register(ctx, req)
	case register.OpUnregister:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.drop(req.Container)
		return nil
	default:
		return fmt.Errorf("unknown op %q", req.Op)
	}
}

// register claims req.Agent for the container and waits until Envoy has
// bound its listeners. A claim that fails is withdrawn.
func (s *Source) register(ctx context.Context, req register.Request) error {
	r, err := s.check(req)
	if err != nil {
		s.Log.Error("refused a hook registration", "container", req.Container, "agent", req.Agent, "err", err)
		return err
	}
	bound := make(chan error, 1)
	if err := s.claim(req.Container, r, bound); err != nil {
		return err
	}
	s.Log.Info("hook registration", "container", req.Container, "agent", r.Agent, "netns", r.Netns)

	select {
	case err = <-bound:
	case <-time.After(s.BoundTimeout):
		err = fmt.Errorf("Envoy did not bind the listeners within %v", s.BoundTimeout)
	case <-ctx.Done():
		err = errors.New("the control plane is stopping")
	}
	if err != nil {
		s.mu.Lock()
		if cur, ok := s.regs[req.Container]; ok && cur == r {
			s.drop(req.Container)
		}
		s.mu.Unlock()
	}
	return err
}

// check validates a register request and reads the namespace's identity.
func (s *Source) check(req register.Request) (registration, error) {
	if !activedir.ValidName(req.Agent) {
		return registration{}, fmt.Errorf("not an agent name: %q", req.Agent)
	}
	if want := register.NetnsPath(s.NetnsDir, req.Container); req.Netns != want {
		return registration{}, fmt.Errorf("the namespace of container %s must be mounted at %s, not %q", req.Container, want, req.Netns)
	}
	allowed, err := s.allowed(req.Agent)
	if err != nil {
		return registration{}, err
	}
	if !allowed {
		return registration{}, fmt.Errorf("%q is not in %s", req.Agent, s.Allowlist)
	}
	id, err := s.Stat(req.Netns)
	if err != nil {
		return registration{}, fmt.Errorf("the container's namespace: %w", err)
	}
	return registration{Agent: req.Agent, Netns: req.Netns, NetnsID: id}, nil
}

// claim records r for container and starts it, unless another live
// container holds the name.
func (s *Source) claim(container string, r registration, bound chan<- error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for other, cur := range s.regs {
		if other == container {
			continue
		}
		if cur.NetnsID == r.NetnsID && s.live(cur) {
			// --network container:<other>, or a pod: not this container's alone.
			return fmt.Errorf("the network namespace of container %s is container %s's (agent %q)", container, other, cur.Agent)
		}
		if cur.Agent != r.Agent {
			continue
		}
		if s.live(cur) {
			return fmt.Errorf("agent %q is registered by container %s", r.Agent, other)
		}
		s.drop(other)
	}
	if old, ok := s.regs[container]; ok && old.Agent != r.Agent {
		// The same container under another name: that name goes.
		s.drop(container)
	}
	s.regs[container] = r
	s.save()
	s.emit(agent.Event{Agent: r.agent(), Bound: bound})
	return nil
}

// allowed reports whether name is a whole line of the allowlist, refusing
// an allowlist another user than AllowlistOwner could have changed
// (allowlist.File).
func (s *Source) allowed(name string) (bool, error) {
	return allowlist.File{Path: s.Allowlist, Owner: s.AllowlistOwner, Root: s.AllowlistRoot, Log: s.Log}.Allowed(name)
}
