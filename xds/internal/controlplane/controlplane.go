// Package controlplane keeps the host Envoy's xDS snapshot in step with the
// running agents: their listeners (LDS), their relay clusters (CDS) and
// their identities (SDS).
//
// One goroutine (Run) owns all state. The agent.Source and the identity
// subscriptions send it their changes on a channel, and it re-reads the
// egress allowlists and the model auth mode on a timer, so nothing is shared
// and nothing is locked. Each change that alters what Envoy would see is
// published as a new snapshot.
package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/cds"
	"github.com/bpalermo/redoma/xds/internal/egress"
	"github.com/bpalermo/redoma/xds/internal/identity"
	"github.com/bpalermo/redoma/xds/internal/lds"
	"github.com/bpalermo/redoma/xds/internal/modelauth"
)

// NodeKey is the snapshot cache's one key: every node that connects is the
// host Envoy, the only client the socket's permissions let in.
const NodeKey = "redoma-host"

// NodeHash maps every node to NodeKey.
type NodeHash struct{}

// ID returns NodeKey.
func (NodeHash) ID(*corev3.Node) string { return NodeKey }

// SecretName is the SDS name of an agent's X.509 SVID.
func SecretName(id string) string { return "agent/" + id }

// BundleSecretName is the SDS name of the trust domain's bundle, a validation
// context: its SPIFFE ID, as SPIRE's own SDS names a bundle.
func BundleSecretName(trustDomain string) string { return "spiffe://" + trustDomain }

// Identity is what the control plane needs of SPIRE (identity.Client).
type Identity interface {
	WatchX509SVID(ctx context.Context, a agent.Agent, update func(*identity.X509SVID))
	WatchX509Bundle(ctx context.Context, update func([]byte))
}

// Config is what the control plane reads besides its Source.
type Config struct {
	// EtcDir has egress/<id>.txt and model-auth (/etc/redoma).
	EtcDir string
	// TrustDomain is the agents' SPIFFE trust domain.
	TrustDomain string
	// ScanInterval is how often the allowlists and the mode are read again.
	ScanInterval time.Duration
	// RebindTimeout is how long a removal of the listeners at a namespace
	// path waits for Envoy to report it applied (see ListenersApplied) before
	// it is published again, for an agent waiting to be bound there.
	RebindTimeout time.Duration
	// RebindGrace is how long after Envoy reports a removal applied the path
	// is still treated as taken: Envoy closes the old sockets only after its
	// workers stop listening, which can come after the report.
	RebindGrace time.Duration
	// StateFile records which namespace Envoy may hold listeners in at each
	// path, across restarts of this process (see state.go). Empty: none.
	StateFile string
	// SyncTimeout is how long the first snapshot waits for the Source to
	// say it has emitted its initial set. Then it publishes what the Source
	// has, unless StateFile recorded listeners Envoy may still hold: then it
	// logs an error and waits another SyncTimeout (see syncTimedOut).
	SyncTimeout time.Duration
	// Relay, if set, gives every agent a third listener, its relay
	// listener, and a cluster to redoma-relay (CDS) once its SVID is there.
	Relay *cds.Relay
}

type running struct {
	agent     agent.Agent
	hosts     []string
	egressErr string
	svid      *identity.X509SVID
	gen       uint64
	cancel    context.CancelFunc
}

// event is a change sent to Run by the Source or a subscription.
type event func(*Controller)

// Controller publishes the snapshot. Create it with New and start it with Run.
type Controller struct {
	cfg    Config
	source agent.Source
	id     Identity
	cache  cache.SnapshotCache
	log    *slog.Logger

	events chan event

	// nonce prefixes every version this process publishes, so a version
	// Envoy reports from an earlier process is never taken for one of ours.
	nonce string
	// The highest listener version of this process Envoy reported having
	// applied, and a wake-up for Run. Written from other goroutines.
	applied atomic.Uint64
	wakeC   chan struct{}

	// Owned by Run.
	agents map[string]*running
	// removedIn is, per namespace path, the removal of the listeners bound
	// there; waiting are the agents to bind at a path not yet clear.
	removedIn map[string]*removal
	waiting   map[string]agent.Agent
	// bound are the starts whose Source wants to know when Envoy has bound
	// them, by agent ID (see agent.Event.Bound).
	bound  map[string]*boundWait
	bundle []byte
	// auth is how every model listener injects the credential, and authErr
	// the error that made it None, if one did.
	auth    modelauth.Mode
	authErr string
	gen     uint64
	version uint64
	dirty   bool
	// synced is set once the Source has emitted its initial set, or once
	// SyncTimeout passed with no recorded listeners (syncTimedOut); nothing
	// is published before.
	synced bool
	// saved is what StateFile holds; loaded is how many paths it held at
	// start.
	saved  map[string]agent.NetnsID
	loaded int
	// after is time.After unless a test replaces it.
	after func(time.Duration) <-chan time.Time
}

// New returns a controller that publishes into c under NodeKey the agents
// source says run.
func New(cfg Config, source agent.Source, id Identity, c cache.SnapshotCache, log *slog.Logger) *Controller {
	return &Controller{
		cfg:       cfg,
		source:    source,
		id:        id,
		cache:     c,
		log:       log,
		events:    make(chan event, 16),
		nonce:     newNonce(),
		wakeC:     make(chan struct{}, 1),
		agents:    map[string]*running{},
		removedIn: map[string]*removal{},
		waiting:   map[string]agent.Agent{},
		bound:     map[string]*boundWait{},
		after:     time.After,
	}
}

// Run publishes until ctx ends or the Source fails, then stops every
// subscription. The first snapshot waits for the Source's initial set, so
// that an Envoy still holding the listeners of a previous run does not see a
// snapshot without them.
func (c *Controller) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		for _, a := range c.agents {
			a.cancel()
		}
		for id := range c.bound {
			c.failBound(id, errors.New("the control plane stopped"))
		}
	}()
	go c.id.WatchX509Bundle(ctx, func(b []byte) {
		c.send(ctx, func(c *Controller) { c.setBundle(b) })
	})
	sourceDone := make(chan error, 1)
	go func() {
		sourceDone <- c.source.Run(ctx, func(e agent.Event) {
			c.send(ctx, func(c *Controller) { c.apply(ctx, e) })
		})
	}()

	c.load()
	c.readModelAuth()
	c.dirty = true
	syncC := c.after(c.cfg.SyncTimeout)
	tick := time.NewTicker(c.cfg.ScanInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-syncC:
			syncC = c.syncTimedOut()
		case err := <-sourceDone:
			return sourceStopped(ctx, err)
		case <-tick.C:
			c.rescan(ctx)
		case <-c.wakeC:
			c.rebindWaiting(ctx)
		case ev := <-c.events:
			ev(c)
		}
		if err := c.publish(ctx); err != nil {
			return err
		}
	}
}

// syncTimedOut decides what SyncTimeout passing without a Synced means, and
// returns the next timeout to wait for, if any. With a record of listeners
// Envoy may still hold from the previous run, publishing the Source's partial
// set would make Envoy drop them: keep waiting, and say so each time. With
// none, Envoy holds nothing to lose and needs a first answer: publish what
// there is.
func (c *Controller) syncTimedOut() <-chan time.Time {
	if c.synced {
		return nil
	}
	if c.loaded > 0 {
		c.log.Error("the agent source has not reported its initial set; still not publishing, so Envoy keeps the listeners of the previous run", "after", c.cfg.SyncTimeout, "recorded_paths", c.loaded)
		return c.after(c.cfg.SyncTimeout)
	}
	c.log.Warn("the agent source has not reported its initial set; publishing what it has", "after", c.cfg.SyncTimeout)
	c.synced, c.dirty = true, true
	return nil
}

// sourceStopped is Run's error when the Source returned err. A Source that
// stops while ctx is live no longer follows the agents: stop rather than
// serve a frozen state.
func sourceStopped(ctx context.Context, err error) error {
	if err == nil && ctx.Err() == nil {
		return errors.New("the agent source stopped")
	}
	return err
}

// rescan re-reads the allowlists and the mode, and moves the waiting agents
// on.
func (c *Controller) rescan(ctx context.Context) {
	c.readModelAuth()
	for _, a := range c.agents {
		c.readEgress(a)
	}
	c.rebindWaiting(ctx)
}

// send hands a change to Run, or drops it once Run is gone.
func (c *Controller) send(ctx context.Context, ev event) {
	select {
	case c.events <- ev:
	case <-ctx.Done():
	}
}

func (c *Controller) stop(a *running) {
	a.cancel()
	delete(c.agents, a.agent.ID)
	// The next snapshot, which publish sets before Run takes anything else,
	// is the one without this agent's listeners.
	c.removedIn[a.agent.Netns] = &removal{version: c.version + 1, since: time.Now(), held: a.agent.NetnsID}
	c.dirty = true
	c.log.Info("agent stopped", "agent", a.agent.ID, "netns", a.agent.Netns)
}

// start subscribes the agent's identity and reads its allowlist.
func (c *Controller) start(ctx context.Context, a agent.Agent) {
	c.gen++
	sub, cancel := context.WithCancel(ctx)
	r := &running{agent: a, gen: c.gen, cancel: cancel}
	c.agents[a.ID] = r
	delete(c.removedIn, a.Netns)
	if b, ok := c.bound[a.ID]; ok {
		// The snapshot publish sets next is the first with its listeners.
		b.version = c.version + 1
	}
	c.dirty = true
	c.log.Info("agent running", "agent", a.ID, "netns", a.Netns)
	c.readEgress(r)
	gen := r.gen
	go c.id.WatchX509SVID(sub, a, func(s *identity.X509SVID) {
		c.send(sub, func(c *Controller) { c.setSVID(a.ID, gen, s) })
	})
}

// readEgress re-reads the agent's allowlist. One that cannot be read leaves
// the agent's egress listener with no allowed host (every CONNECT a 403)
// until it is fixed: an error never widens what an agent can reach, and its
// model listener stays.
func (c *Controller) readEgress(a *running) {
	hosts, err := egress.Hosts(c.cfg.EtcDir, a.agent.ID)
	errText := ""
	if err != nil {
		hosts, errText = nil, err.Error()
	}
	if errText != a.egressErr {
		a.egressErr = errText
		if err != nil {
			c.log.Error("egress allowlist unreadable; the agent may tunnel nowhere", "agent", a.agent.ID, "err", err)
		}
	}
	if !slices.Equal(hosts, a.hosts) {
		a.hosts = hosts
		c.dirty = true
	}
}

// readModelAuth re-reads the mode. One that cannot be read or is not known
// leaves every model listener with no credential (the API answers 401) until
// it is fixed: an error never sends the credential in a header it was not
// meant for.
func (c *Controller) readModelAuth() {
	auth, err := modelauth.Read(c.cfg.EtcDir)
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if errText != c.authErr {
		c.authErr = errText
		if err != nil {
			c.log.Error("model auth mode unreadable or unknown; no model listener carries a credential, every model request gets a 401", "err", err)
		}
	}
	if auth != c.auth {
		c.auth = auth
		c.dirty = true
		c.log.Info("model auth mode", "mode", auth.String())
	}
}

// setSVID records an SVID, unless it comes from the subscription of an agent
// that has stopped since (and maybe started again, with a new generation).
func (c *Controller) setSVID(id string, gen uint64, s *identity.X509SVID) {
	a, ok := c.agents[id]
	if !ok || a.gen != gen {
		return
	}
	a.svid = s
	c.dirty = true
	if s == nil {
		c.log.Warn("SPIRE has no SVID for the agent", "agent", id)
		return
	}
	c.log.Info("agent SVID", "agent", id, "id", s.ID, "expires", s.ExpiresAt)
}

func (c *Controller) setBundle(b []byte) {
	c.bundle = b
	c.dirty = true
}

// publish sets a new snapshot if anything changed since the last one.
func (c *Controller) publish(ctx context.Context) error {
	if !c.dirty || !c.synced {
		return nil
	}
	// Before Envoy can bind anything new: a record that lags the snapshot
	// could miss a namespace Envoy holds.
	c.save()
	c.version++
	snap, err := cache.NewSnapshot(c.versionString(c.version), map[resource.Type][]types.Resource{
		resource.ListenerType: c.listeners(),
		resource.ClusterType:  c.clusters(),
		resource.SecretType:   c.secrets(),
	})
	if err != nil {
		return err
	}
	if err := c.cache.SetSnapshot(ctx, NodeKey, snap); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

func (c *Controller) listeners() []types.Resource {
	var out []types.Resource
	for _, id := range slices.Sorted(maps.Keys(c.agents)) {
		a := c.agents[id]
		for _, l := range lds.Listeners(a.agent.Netns, id, c.auth, a.hosts) {
			out = append(out, l)
		}
		if c.cfg.Relay != nil {
			out = append(out, lds.RelayListener(a.agent.Netns, id))
		}
	}
	return out
}

// clusters are the agents' relay clusters: an agent's once its SVID and the
// bundle are in the snapshot, so that Envoy never warms a cluster waiting
// for a secret that is not there.
func (c *Controller) clusters() []types.Resource {
	if c.cfg.Relay == nil || c.bundle == nil {
		return nil
	}
	var out []types.Resource
	for _, id := range slices.Sorted(maps.Keys(c.agents)) {
		if c.agents[id].svid == nil {
			continue
		}
		out = append(out, cds.RelayCluster(id, *c.cfg.Relay, SecretName(id), BundleSecretName(c.cfg.TrustDomain)))
	}
	return out
}

func inline(b []byte) *corev3.DataSource {
	return &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: b}}
}

func (c *Controller) secrets() []types.Resource {
	var out []types.Resource
	for _, id := range slices.Sorted(maps.Keys(c.agents)) {
		s := c.agents[id].svid
		if s == nil {
			continue
		}
		out = append(out, &tlsv3.Secret{
			Name: SecretName(id),
			Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
				CertificateChain: inline(s.CertChainPEM),
				PrivateKey:       inline(s.KeyPEM),
			}},
		})
	}
	if c.bundle != nil {
		out = append(out, &tlsv3.Secret{
			Name: BundleSecretName(c.cfg.TrustDomain),
			Type: &tlsv3.Secret_ValidationContext{ValidationContext: &tlsv3.CertificateValidationContext{
				TrustedCa: inline(c.bundle),
			}},
		})
	}
	return out
}
