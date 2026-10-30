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
//
// A register comes with a pidfd of the container's process, which the hook
// bound to it (hook.BindProcess): the agent's agent.Process, which the
// Broker API names it by. The pidfds are kept in Store, under their
// container's ID, so a restart of redoma-xds gets them back (Stored).
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
	"github.com/bpalermo/redoma/common/pidfd"
	"github.com/bpalermo/redoma/common/register"
	"github.com/bpalermo/redoma/xds/internal/activedir"
	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/fdstore"
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

	// Store keeps each registration's pidfd across restarts of this
	// process, named after its container; nil keeps them in memory only.
	Store fdstore.Store
	// Stored are the descriptors Store passed back at start, by name. Run
	// takes them: a registration gets the one of its name when exactly one
	// is a live pidfd, and the others are closed and removed from Store.
	Stored map[string][]*os.File

	// PeerUID, Stat and ReadRequest are the system's unless a test replaces
	// them.
	PeerUID     func(net.Conn) (uint32, error)
	Stat        activedir.StatFunc
	ReadRequest func(net.Conn) (register.Request, *os.File, error)

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
	// proc is the container's process, nil when the hook sent none or it
	// was lost. Not recorded: Store keeps it.
	proc *pidfd.FD
}

func (r registration) agent() agent.Agent {
	a := agent.Agent{ID: r.Agent, Netns: r.Netns, NetnsID: r.NetnsID, Selectors: Selectors(r.Agent)}
	if r.proc != nil {
		a.Process = r.proc
	}
	return a
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
	if s.ReadRequest == nil {
		s.ReadRequest = register.ReadRequest
	}
	s.mu.Lock()
	s.emit = emit
	s.regs = s.load()
	s.restoreProcesses()
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

// restoreProcesses gives each registration the pidfd Stored holds under its
// container's ID, when exactly one of them is a live pidfd: a container
// restarted under the same ID may have left an older one. Every name is
// then removed from Store and the one kept added back, so Store holds
// exactly the registrations' pidfds. Called with mu held.
func (s *Source) restoreProcesses() {
	for name, files := range s.Stored {
		live := livePidfds(files)
		r, ok := s.regs[name]
		switch {
		case ok && len(live) == 1:
			r.proc = live[0]
			s.regs[name] = r
		case ok && len(live) > 1:
			s.Log.Error("several live pidfds stored for one container; keeping none", "container", name, "count", len(live))
			closeAll(live)
		default:
			closeAll(live)
		}
		s.removeStored(name)
	}
	s.Stored = nil
	bound := 0
	for _, r := range s.regs {
		if r.proc != nil {
			bound++
		}
	}
	s.Log.Info("hook registrations read back", "registrations", len(s.regs), "with_pidfd", bound, "without_pidfd", len(s.regs)-bound)
	for container, r := range s.regs {
		if r.proc == nil {
			s.Log.Warn("hook registration without its container's pidfd; in Broker mode the agent gets no SVID until its container is restarted", "container", container, "agent", r.Agent)
			continue
		}
		s.addStored(container, r.proc)
	}
}

// livePidfds are the files that are pidfds of live processes; the others
// are closed.
func livePidfds(files []*os.File) []*pidfd.FD {
	var live []*pidfd.FD
	for _, f := range files {
		p, err := pidfd.New(f)
		if err != nil {
			continue
		}
		if !p.Alive() {
			p.Close()
			continue
		}
		live = append(live, p)
	}
	return live
}

func closeAll(procs []*pidfd.FD) {
	for _, p := range procs {
		p.Close()
	}
}

// addStored keeps proc in Store under container's ID, after removing
// anything an earlier run of the container left there.
func (s *Source) addStored(container string, proc *pidfd.FD) {
	if s.Store == nil {
		return
	}
	s.removeStored(container)
	if err := s.Store.Add(container, proc.File()); err != nil && !errors.Is(err, fdstore.ErrNoStore) {
		s.Log.Warn("cannot keep the container's pidfd in systemd's store; it is lost if redoma xds restarts", "container", container, "err", err)
	}
}

func (s *Source) removeStored(container string) {
	if s.Store == nil {
		return
	}
	if err := s.Store.Remove(container); err != nil && !errors.Is(err, fdstore.ErrNoStore) {
		s.Log.Warn("cannot remove the container's pidfd from systemd's store", "container", container, "err", err)
	}
}

// put records r for container, replacing what the container had: its
// pidfd, if another, is closed, and r's kept in Store. Called with mu held.
func (s *Source) put(container string, r registration) {
	if old, ok := s.regs[container]; ok && old.proc != nil && old.proc != r.proc {
		old.proc.Close()
	}
	s.regs[container] = r
	if r.proc != nil {
		s.addStored(container, r.proc)
	} else {
		s.removeStored(container)
	}
	s.save()
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
	if r.proc != nil {
		r.proc.Close()
	}
	s.removeStored(container)
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
	req, file, err := s.ReadRequest(conn)
	if err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	if file != nil && req.Op != register.OpRegister {
		file.Close()
		return fmt.Errorf("a pidfd with a %s", req.Op)
	}
	if !register.ValidContainer(req.Container) {
		if file != nil {
			file.Close()
		}
		return fmt.Errorf("not a container ID: %q", req.Container)
	}
	switch req.Op {
	case register.OpRegister:
		return s.register(ctx, req, file)
	case register.OpUnregister:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.drop(req.Container)
		return nil
	default:
		return fmt.Errorf("unknown op %q", req.Op)
	}
}

// register claims req.Agent for the container, with the pidfd of its
// process if file is one, and waits until Envoy has bound its listeners. A
// claim that fails is withdrawn.
func (s *Source) register(ctx context.Context, req register.Request, file *os.File) error {
	r, err := s.check(req)
	if err == nil && file != nil {
		r.proc, err = process(file)
		file = nil
	}
	if file != nil {
		file.Close()
	}
	if err != nil {
		s.Log.Error("refused a hook registration", "container", req.Container, "agent", req.Agent, "err", err)
		return err
	}
	bound := make(chan error, 1)
	if err := s.claim(req.Container, r, bound); err != nil {
		if r.proc != nil {
			r.proc.Close()
		}
		return err
	}
	s.Log.Info("hook registration", "container", req.Container, "agent", r.Agent, "netns", r.Netns, "pidfd", r.proc != nil)
	if r.proc == nil {
		s.Log.Warn("hook registration without its container's pidfd (the hook could not bind the process); in Broker mode the agent gets no SVID", "container", req.Container, "agent", r.Agent)
	}

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

// process takes the pidfd a register came with: a pidfd, of a process
// still alive.
func process(file *os.File) (*pidfd.FD, error) {
	p, err := pidfd.New(file)
	if err != nil {
		return nil, fmt.Errorf("the container's process: %w", err)
	}
	if !p.Alive() {
		p.Close()
		return nil, errors.New("the container's process has exited")
	}
	return p, nil
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
	s.put(container, r)
	s.emit(agent.Event{Agent: r.agent(), Bound: bound})
	return nil
}

// allowed reports whether name is a whole line of the allowlist, refusing
// an allowlist another user than AllowlistOwner could have changed
// (allowlist.File).
func (s *Source) allowed(name string) (bool, error) {
	return allowlist.File{Path: s.Allowlist, Owner: s.AllowlistOwner, Root: s.AllowlistRoot, Log: s.Log}.Allowed(name)
}
