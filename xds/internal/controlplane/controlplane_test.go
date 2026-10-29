package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	pb "google.golang.org/protobuf/proto"

	"github.com/bpalermo/redoma/xds/internal/activedir"
	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/identity"
	"github.com/bpalermo/redoma/xds/internal/identity/identitytest"
	"github.com/bpalermo/redoma/xds/internal/lds"
	"github.com/bpalermo/redoma/xds/internal/modelauth"
)

const td = "redoma.internal"

// env is a launcher's state in a temporary directory read by the activedir
// Source, a fake SPIRE and a running controller.
type env struct {
	cfg   Config
	src   *activedir.Source
	fake  *identitytest.Server
	ca    *identitytest.CA
	cache cache.SnapshotCache
	done  chan error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith is newEnv with the config changed by mod, if set.
func newEnvWith(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	e := &env{
		cfg: Config{
			EtcDir:        filepath.Join(dir, "etc/redoma"),
			TrustDomain:   td,
			ScanInterval:  10 * time.Millisecond,
			RebindTimeout: time.Hour,
			RebindGrace:   5 * time.Millisecond,
			SyncTimeout:   time.Hour,
		},
		src: &activedir.Source{
			ActiveDir: filepath.Join(dir, "run/redoma/active"),
			NetnsDir:  filepath.Join(dir, "run/netns"),
			Interval:  10 * time.Millisecond,
			Log:       log,
		},
	}
	if mod != nil {
		mod(&e.cfg)
	}
	for _, d := range []string{e.src.ActiveDir, e.src.NetnsDir, filepath.Join(e.cfg.EtcDir, "egress")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	fake, conn := identitytest.Serve(t)
	e.fake = fake
	e.ca = identitytest.NewCA(t, td)
	e.fake.SetBundle("spiffe://"+td, e.ca.DER())
	id := identity.New(conn, td, log)
	id.MinBackoff, id.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond

	e.cache = cache.NewSnapshotCache(true, NodeHash{}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	e.done = make(chan error, 1)
	ctl := New(e.cfg, e.src, id, e.cache, log)
	go func() { e.done <- ctl.Run(ctx) }()
	go ackLikeEnvoy(ctx, e.cache, ctl)
	t.Cleanup(func() {
		cancel()
		if err := <-e.done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return e
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startAgent does what the launcher does: the namespace, then the active
// file; and registers the agent's SVID with the fake SPIRE.
func (e *env) startAgent(t *testing.T, repo, egress string) {
	t.Helper()
	if egress != "" {
		write(t, filepath.Join(e.cfg.EtcDir, "egress", repo+".txt"), egress)
	}
	e.fake.SetSVIDs("redoma:agent:"+repo, e.ca.SVID(t, "/agent/"+repo))
	write(t, activedir.Namespace(e.src.NetnsDir, repo), "")
	write(t, filepath.Join(e.src.ActiveDir, repo), "")
}

func (e *env) stopAgent(t *testing.T, repo string) {
	t.Helper()
	for _, p := range []string{filepath.Join(e.src.ActiveDir, repo), activedir.Namespace(e.src.NetnsDir, repo)} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
}

type view struct {
	listeners []string
	secrets   []string
	snap      cache.ResourceSnapshot
}

func (e *env) view() view {
	snap, err := e.cache.GetSnapshot(NodeKey)
	if err != nil {
		return view{}
	}
	return view{
		listeners: slices.Sorted(maps.Keys(snap.GetResources(resource.ListenerType))),
		secrets:   slices.Sorted(maps.Keys(snap.GetResources(resource.SecretType))),
		snap:      snap,
	}
}

// eventually waits until a snapshot is published and cond holds of it.
// Before the first one there is nothing to look at: cond never sees a view
// without a snapshot.
func (e *env) eventually(t *testing.T, what string, cond func(view) bool) view {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := e.view()
		if v.snap != nil && cond(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; listeners %q, secrets %q", what, v.listeners, v.secrets)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPublishesNothingBeforeAnyAgent(t *testing.T) {
	e := newEnv(t)
	v := e.eventually(t, "the bundle", func(v view) bool { return slices.Equal(v.secrets, []string{"spiffe://" + td}) })
	if len(v.listeners) != 0 {
		t.Errorf("listeners %q with no agent running", v.listeners)
	}
}

func TestAgentLifecycle(t *testing.T) {
	e := newEnv(t)
	e.startAgent(t, "beta", "github.com\n")
	e.startAgent(t, "alpha", "")

	wantListeners := []string{
		"agent_alpha_egress", "agent_alpha_model",
		"agent_beta_egress", "agent_beta_model",
	}
	wantSecrets := []string{"agent/alpha", "agent/beta", "spiffe://" + td}
	v := e.eventually(t, "both agents", func(v view) bool {
		return slices.Equal(v.listeners, wantListeners) && slices.Equal(v.secrets, wantSecrets)
	})

	// The listeners are the ones lds builds, from the agent's allowlist.
	got := v.snap.GetResources(resource.ListenerType)["agent_beta_egress"].(*listenerv3.Listener)
	want := lds.Listeners(activedir.Namespace(e.src.NetnsDir, "beta"), "beta", modelauth.APIKey, []string{"github.com"})[1]
	if !pb.Equal(got, want) {
		t.Error("beta's egress listener is not lds.Listeners' for its allowlist")
	}
	secret := v.snap.GetResources(resource.SecretType)["agent/beta"].(*tlsv3.Secret)
	if len(secret.GetTlsCertificate().GetCertificateChain().GetInlineBytes()) == 0 ||
		len(secret.GetTlsCertificate().GetPrivateKey().GetInlineBytes()) == 0 {
		t.Error("agent/beta has no certificate or key")
	}

	// An agent going away takes its listeners and its secret with it, and its
	// subscription is closed.
	e.stopAgent(t, "beta")
	e.eventually(t, "beta gone", func(v view) bool {
		return slices.Equal(v.listeners, []string{"agent_alpha_egress", "agent_alpha_model"}) &&
			slices.Equal(v.secrets, []string{"agent/alpha", "spiffe://" + td})
	})
	waitStreams(t, e.fake, "redoma:agent:beta", 0)
	waitStreams(t, e.fake, "redoma:agent:alpha", 1)

	// It comes back: listeners and secret again, on a new subscription.
	e.startAgent(t, "beta", "")
	e.eventually(t, "beta back", func(v view) bool {
		return slices.Equal(v.listeners, wantListeners) && slices.Equal(v.secrets, wantSecrets)
	})
	waitStreams(t, e.fake, "redoma:agent:beta", 1)
}

// TestNoListenersWithoutNamespace: an active file whose namespace is not
// there (yet) gets nothing; Envoy could not bind its listeners.
func TestNoListenersWithoutNamespace(t *testing.T) {
	e := newEnv(t)
	write(t, filepath.Join(e.src.ActiveDir, "a"), "")
	e.startAgent(t, "b", "")
	v := e.eventually(t, "b", func(v view) bool { return slices.Contains(v.listeners, "agent_b_model") })
	if slices.Contains(v.listeners, "agent_a_model") {
		t.Error("listeners for an agent with no namespace")
	}
}

// TestEgressAllowlistChanges: an edited allowlist is picked up at the next
// scan, and one that cannot be parsed allows nothing.
func TestEgressAllowlistChanges(t *testing.T) {
	e := newEnv(t)
	e.startAgent(t, "a", "github.com\n")
	domains := func(v view) []string {
		if v.snap == nil {
			return nil
		}
		r, ok := v.snap.GetResources(resource.ListenerType)["agent_a_egress"]
		if !ok {
			return nil
		}
		want := r.(*listenerv3.Listener)
		for _, l := range [][]string{{"github.com"}, {"github.com", "example.com"}, nil} {
			if pb.Equal(want, lds.Listeners(activedir.Namespace(e.src.NetnsDir, "a"), "a", modelauth.APIKey, l)[1]) {
				return append([]string{"match"}, l...)
			}
		}
		return []string{"unknown"}
	}
	e.eventually(t, "github.com only", func(v view) bool { return slices.Equal(domains(v), []string{"match", "github.com"}) })

	write(t, filepath.Join(e.cfg.EtcDir, "egress", "a.txt"), "github.com\nexample.com\n")
	e.eventually(t, "example.com added", func(v view) bool {
		return slices.Equal(domains(v), []string{"match", "github.com", "example.com"})
	})

	write(t, filepath.Join(e.cfg.EtcDir, "egress", "a.txt"), "github.com\n*.example.com\n")
	v := e.eventually(t, "nothing allowed", func(v view) bool { return slices.Equal(domains(v), []string{"match"}) })
	if !slices.Contains(v.listeners, "agent_a_model") {
		t.Error("a bad allowlist took the model listener down")
	}
}

// TestEgressDefaultsChanges: the defaults' file is read at the next scan for
// every agent, before each one's own hosts, and a bad line in it allows
// nothing to anyone.
func TestEgressDefaultsChanges(t *testing.T) {
	e := newEnv(t)
	e.startAgent(t, "a", "github.com\n")
	e.startAgent(t, "b", "example.com\n")
	allows := func(v view, repo string, hosts []string) bool {
		if v.snap == nil {
			return false
		}
		r, ok := v.snap.GetResources(resource.ListenerType)["agent_"+repo+"_egress"]
		return ok && pb.Equal(r.(*listenerv3.Listener), lds.Listeners(activedir.Namespace(e.src.NetnsDir, repo), repo, modelauth.APIKey, hosts)[1])
	}
	e.eventually(t, "own hosts only", func(v view) bool {
		return allows(v, "a", []string{"github.com"}) && allows(v, "b", []string{"example.com"})
	})

	defaults := filepath.Join(e.cfg.EtcDir, "egress", "_default.txt")
	write(t, defaults, "cache.example.org\ngithub.com\n")
	e.eventually(t, "the defaults first, each host once", func(v view) bool {
		return allows(v, "a", []string{"cache.example.org", "github.com"}) &&
			allows(v, "b", []string{"cache.example.org", "github.com", "example.com"})
	})

	write(t, defaults, "cache.example.org:443\n")
	e.eventually(t, "nothing allowed", func(v view) bool { return allows(v, "a", nil) && allows(v, "b", nil) })

	if err := os.Remove(defaults); err != nil {
		t.Fatal(err)
	}
	e.eventually(t, "own hosts once the defaults are gone", func(v view) bool {
		return allows(v, "a", []string{"github.com"}) && allows(v, "b", []string{"example.com"})
	})
}

// TestModelAuthChanges: the mode file is read at the next scan, and a change
// is a new listener version; a mode that is not known injects no credential,
// and a missing file is api-key again.
func TestModelAuthChanges(t *testing.T) {
	e := newEnv(t)
	e.startAgent(t, "a", "github.com\n")
	mode := func(v view) string {
		r, ok := v.snap.GetResources(resource.ListenerType)["agent_a_model"]
		if !ok {
			return ""
		}
		for _, auth := range []modelauth.Mode{modelauth.APIKey, modelauth.Bearer, modelauth.None} {
			if pb.Equal(r.(*listenerv3.Listener), lds.Listeners(activedir.Namespace(e.src.NetnsDir, "a"), "a", auth, nil)[0]) {
				return auth.String()
			}
		}
		return "unknown"
	}
	path := modelauth.Path(e.cfg.EtcDir)
	version := e.eventually(t, "api-key with no file", func(v view) bool { return mode(v) == "api-key" }).
		snap.GetVersion(resource.ListenerType)

	for _, step := range []struct{ content, want string }{
		{"bearer\n", "bearer"},
		{"Bearer sk-ant-oat01-x\n", "none"},
		{" api-key ", "api-key"},
		{"bearer", "bearer"},
	} {
		write(t, path, step.content)
		v := e.eventually(t, step.want, func(v view) bool { return mode(v) == step.want })
		if got := v.snap.GetVersion(resource.ListenerType); got == version {
			t.Errorf("%q: listeners changed under the same version %q", step.content, got)
		}
		version = v.snap.GetVersion(resource.ListenerType)
		if !slices.Contains(v.listeners, "agent_a_egress") {
			t.Errorf("%q: the egress listener went away", step.content)
		}
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e.eventually(t, "api-key once the file is gone", func(v view) bool { return mode(v) == "api-key" })
}

// TestModelAuthUnknownAtStart: a bad mode at start is no credential from the
// first snapshot on.
func TestModelAuthUnknownAtStart(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	_, conn := identitytest.Serve(t)
	etc := t.TempDir()
	write(t, modelauth.Path(etc), "x-api-key\n")

	src := &chanSource{events: make(chan agent.Event), err: make(chan error)}
	e := &env{cache: cache.NewSnapshotCache(true, NodeHash{}, nil)}
	// No rescan in the test's time: the mode is the one read at start.
	cfg := Config{EtcDir: etc, TrustDomain: td, ScanInterval: time.Hour}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- New(cfg, src, identity.New(conn, td, log), e.cache, log).Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	src.events <- agent.Event{Agent: agent.Agent{ID: "a", Netns: "/run/netns/redoma-a"}}
	v := e.eventually(t, "a's listeners", func(v view) bool { return slices.Contains(v.listeners, "agent_a_model") })
	got := v.snap.GetResources(resource.ListenerType)["agent_a_model"].(*listenerv3.Listener)
	if !pb.Equal(got, lds.Listeners("/run/netns/redoma-a", "a", modelauth.None, nil)[0]) {
		t.Error("the model listener of a bad mode is not the one with no credential")
	}
}

// TestSVIDRotation: a new SVID from SPIRE replaces the agent's secret.
func TestSVIDRotation(t *testing.T) {
	e := newEnv(t)
	e.startAgent(t, "a", "")
	chain := func(v view) string {
		if v.snap == nil {
			return ""
		}
		r, ok := v.snap.GetResources(resource.SecretType)["agent/a"]
		if !ok {
			return ""
		}
		return string(r.(*tlsv3.Secret).GetTlsCertificate().GetCertificateChain().GetInlineBytes())
	}
	first := chain(e.eventually(t, "a's secret", func(v view) bool { return chain(v) != "" }))
	e.fake.SetSVIDs("redoma:agent:a", e.ca.SVID(t, "/agent/a"))
	e.eventually(t, "a rotated", func(v view) bool { return chain(v) != "" && chain(v) != first })

	// SPIRE drops the entry: the secret goes, the listeners stay.
	e.fake.SetSVIDs("redoma:agent:a")
	v := e.eventually(t, "a's secret gone", func(v view) bool { return chain(v) == "" })
	if !slices.Contains(v.listeners, "agent_a_model") {
		t.Error("the listeners went with the SVID")
	}
}

func waitStreams(t *testing.T, fake *identitytest.Server, selector string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for fake.Streams(selector) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d stream(s) open, want %d", selector, fake.Streams(selector), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// chanSource is a Source whose events the test sends. It reports its
// initial set (empty) at once, unless noSync.
type chanSource struct {
	events chan agent.Event
	err    chan error
	noSync bool
}

func (s *chanSource) Run(ctx context.Context, emit func(agent.Event)) error {
	if !s.noSync {
		emit(agent.Event{Synced: true})
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.err:
			return err
		case e := <-s.events:
			emit(e)
		}
	}
}

// TestNamespaceAndSelectorsComeFromTheSource: nothing about an agent's
// namespace or selectors is derived from its name; a Source that is told a
// container's own namespace (an OCI hook) gets its listeners bound there and
// its identity subscribed by its selectors. And a Source that fails stops
// the control plane.
func TestNamespaceAndSelectorsComeFromTheSource(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	fake, conn := identitytest.Serve(t)
	ca := identitytest.NewCA(t, td)
	sel := agent.Selector{Type: "podman", Value: "label:org.example/agent:x"}
	fake.SetSVIDs(sel.Type+":"+sel.Value, ca.SVID(t, "/agent/x"))
	id := identity.New(conn, td, log)

	src := &chanSource{events: make(chan agent.Event), err: make(chan error)}
	e := &env{cache: cache.NewSnapshotCache(true, NodeHash{}, nil)}
	cfg := Config{EtcDir: t.TempDir(), TrustDomain: td, ScanInterval: 10 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- New(cfg, src, id, e.cache, log).Run(t.Context()) }()

	netns := "/run/containers/0123abcd/netns"
	src.events <- agent.Event{Agent: agent.Agent{ID: "x", Netns: netns, Selectors: []agent.Selector{sel}}}
	v := e.eventually(t, "x's listeners and secret", func(v view) bool {
		return slices.Contains(v.listeners, "agent_x_model") && slices.Contains(v.secrets, "agent/x")
	})
	l := v.snap.GetResources(resource.ListenerType)["agent_x_model"].(*listenerv3.Listener)
	if got := l.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath(); got != netns {
		t.Errorf("bound in %q, want the Source's %q", got, netns)
	}

	// The same agent in a new namespace replaces the old one.
	src.events <- agent.Event{Agent: agent.Agent{ID: "x", Netns: netns + "2", Selectors: []agent.Selector{sel}}}
	e.eventually(t, "x moved", func(v view) bool {
		r, ok := v.snap.GetResources(resource.ListenerType)["agent_x_model"]
		return ok && r.(*listenerv3.Listener).GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath() == netns+"2"
	})
	waitStreams(t, fake, sel.Type+":"+sel.Value, 1)

	src.err <- errors.New("registrar gone")
	select {
	case err := <-done:
		if err == nil || err.Error() != "registrar gone" {
			t.Errorf("Run = %v, want the Source's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop when its Source failed")
	}
}
