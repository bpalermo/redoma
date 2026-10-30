package hub

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
	"github.com/bpalermo/redoma/common/reliable"
	"github.com/bpalermo/redoma/relay/internal/mtls"
	"github.com/bpalermo/redoma/relay/internal/mtls/mtlstest"
)

const td = "redoma.internal"

// relay is a running hub: the agents' service over mutual TLS and the
// lead's, both in memory.
type relay struct {
	h      *Hub
	agents *bufconn.Listener
	lead   relayv1.LeadClient
	stop   func()
}

type env struct {
	t   *testing.T
	ca  *mtlstest.CA
	dir string

	mu    sync.Mutex
	allow map[string]bool
}

func newEnv(t *testing.T) *env {
	return &env{t: t, ca: mtlstest.NewCA(t, td), dir: t.TempDir(), allow: map[string]bool{"beta": true, "alpha": true}}
}

func (e *env) allowed(name string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.allow[name], nil
}

// start runs a hub on the env's state directory, until the test ends or
// stop.
func (e *env) start() *relay {
	t := e.t
	t.Helper()
	h, err := New(Config{Dir: e.dir, TrustDomain: td, Allowed: e.allowed, AckEvery: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := mtls.ServerConfig(e.ca.Source(t, "spiffe://"+td+"/host/relay"), td, e.allowed)
	if err != nil {
		t.Fatal(err)
	}
	ag := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)), grpc.MaxRecvMsgSize(MaxRecvMsgSize), grpc.MaxConcurrentStreams(1))
	relayv1.RegisterRelayServer(ag, h.RelayServer())
	ld := grpc.NewServer()
	relayv1.RegisterLeadServer(ld, h.LeadServer())
	al, ll := bufconn.Listen(1<<20), bufconn.Listen(1<<20)
	go func() { _ = ag.Serve(al) }()
	go func() { _ = ld.Serve(ll) }()
	conn := dial(t, ll, insecure.NewCredentials())
	var once sync.Once
	stop := func() {
		once.Do(func() {
			conn.Close()
			ag.Stop()
			ld.Stop()
			h.Close()
		})
	}
	t.Cleanup(stop)
	return &relay{h: h, agents: al, lead: relayv1.NewLeadClient(conn), stop: stop}
}

func dial(t *testing.T, l *bufconn.Listener, creds credentials.TransportCredentials) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///relay",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }),
		grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// agent is a fake shim: a raw Connect stream with an SVID.
type agent struct {
	t      *testing.T
	st     relayv1.Relay_ConnectClient
	cancel context.CancelFunc
	epoch  string
	relay  string
	seq    uint64
}

// connect opens a stream as the SVID of id, with the queue epoch epoch.
func (e *env) connect(r *relay, id, epoch string) (*agent, error) {
	t := e.t
	t.Helper()
	var certs []tls.Certificate
	if id != "" {
		certs = []tls.Certificate{e.ca.Issue(t, id)}
	}
	// The test client does not check the relay: the server's checks are
	// what is tested.
	conn := dial(t, r.agents, credentials.NewTLS(&tls.Config{Certificates: certs, InsecureSkipVerify: true}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if epoch != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, reliable.EpochKey, epoch)
	}
	st, err := relayv1.NewRelayClient(conn).Connect(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	md, err := st.Header()
	if err != nil {
		cancel()
		return nil, err
	}
	a := &agent{t: t, st: st, cancel: cancel, epoch: epoch}
	if v := md.Get(reliable.EpochKey); len(v) == 1 {
		a.relay = v[0]
	}
	return a, nil
}

func (e *env) mustConnect(r *relay, name, epoch string) *agent {
	e.t.Helper()
	a, err := e.connect(r, "spiffe://"+td+"/agent/"+name, epoch)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

// recv returns the next envelope that is not an Ack.
func (a *agent) recv() *relayv1.Envelope {
	a.t.Helper()
	type res struct {
		e   *relayv1.Envelope
		err error
	}
	c := make(chan res, 1)
	go func() {
		for {
			e, err := a.st.Recv()
			if err != nil || e.GetAck() == nil {
				c <- res{e, err}
				return
			}
		}
	}()
	select {
	case r := <-c:
		if r.err != nil {
			a.t.Fatalf("recv: %v", r.err)
		}
		return r.e
	case <-time.After(5 * time.Second):
		a.t.Fatal("recv: timed out")
		return nil
	}
}

// endErr waits for the stream to end and returns its status.
func (a *agent) endErr() error {
	a.t.Helper()
	c := make(chan error, 1)
	go func() {
		for {
			if _, err := a.st.Recv(); err != nil {
				c <- err
				return
			}
		}
	}()
	select {
	case err := <-c:
		return err
	case <-time.After(5 * time.Second):
		a.t.Fatal("the stream did not end")
		return nil
	}
}

func (a *agent) send(e *relayv1.Envelope) {
	a.t.Helper()
	a.seq++
	e.SetSeq(a.seq)
	if err := a.st.Send(e); err != nil {
		a.t.Fatal(err)
	}
}

func (a *agent) ack(seq uint64) {
	a.t.Helper()
	if err := a.st.Send(relayv1.Envelope_builder{Ack: relayv1.Ack_builder{Seq: seq}.Build()}.Build()); err != nil {
		a.t.Fatal(err)
	}
}

func watch(t *testing.T, r *relay, agent string) (<-chan *relayv1.WatchEvent, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := r.lead.Watch(ctx, relayv1.WatchRequest_builder{Agent: agent}.Build())
	if err != nil {
		t.Fatal(err)
	}
	c := make(chan *relayv1.WatchEvent, 64)
	go func() {
		defer close(c)
		for {
			ev, err := st.Recv()
			if err != nil {
				return
			}
			c <- ev
		}
	}()
	return c, cancel
}

func next(t *testing.T, c <-chan *relayv1.WatchEvent) *relayv1.WatchEvent {
	t.Helper()
	select {
	case ev, ok := <-c:
		if !ok {
			t.Fatal("the watch ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no watch event")
		return nil
	}
}

func none(t *testing.T, c <-chan *relayv1.WatchEvent) {
	t.Helper()
	select {
	case ev := <-c:
		t.Fatalf("an unexpected watch event: %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func pending(r *relay, name string) int {
	a, ok := r.h.known(name)
	if !ok {
		return 0
	}
	return a.out.Len()
}

// TestNewerStreamReplaces: a second stream of the same agent replaces the
// first, which ends Aborted, and takes over the queue.
func TestNewerStreamReplaces(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	old := e.mustConnect(r, "beta", "shim-1")
	// The first stream is attached once the relay has sent its header.
	fresh := e.mustConnect(r, "beta", "shim-1")
	if err := old.endErr(); status.Code(err) != codes.Aborted {
		t.Errorf("the older stream ended with %v, want Aborted", err)
	}
	if _, err := r.lead.Send(context.Background(), relayv1.SendRequest_builder{Agent: "beta", Text: "x"}.Build()); err != nil {
		t.Fatal(err)
	}
	if got := fresh.recv(); got.GetTask().GetText() != "x" {
		t.Errorf("the newer stream got %v", got)
	}
}

// TestStreamRefusals: a stream without an SVID of an allowed agent, or
// without an epoch, is refused.
func TestStreamRefusals(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	for name, id := range map[string]string{
		"no client certificate": "",
		"not in the allowlist":  "spiffe://" + td + "/agent/someone-else",
		"another path":          "spiffe://" + td + "/host/relay",
		"another trust domain":  "spiffe://example.org/agent/beta",
	} {
		if a, err := e.connect(r, id, "shim-1"); err == nil {
			err = a.endErr()
			if status.Code(err) != codes.Unavailable && status.Code(err) != codes.Unauthenticated {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	a, err := e.connect(r, "spiffe://"+td+"/agent/beta", "")
	if err == nil {
		err = a.endErr()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("no epoch: %v, want InvalidArgument", err)
	}

	// An agent removed from the allowlist after it connected is refused
	// its next stream on the same certificate.
	e.mu.Lock()
	delete(e.allow, "alpha")
	e.mu.Unlock()
	if a, err := e.connect(r, "spiffe://"+td+"/agent/alpha", "shim-1"); err == nil {
		if err := a.endErr(); status.Code(err) == codes.OK {
			t.Error("a removed agent connected")
		}
	}
}

// TestResendAfterDisconnect: a task delivered on a stream that broke before
// the ack is delivered again on the next one, with the same seq.
func TestResendAfterDisconnect(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	a := e.mustConnect(r, "beta", "shim-1")
	sent, err := r.lead.Send(context.Background(), relayv1.SendRequest_builder{Agent: "beta", Text: "one"}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := a.recv(); got.GetTask().GetId() != sent.GetTaskId() {
		t.Fatalf("got %v", got)
	}
	a.cancel()
	a = e.mustConnect(r, "beta", "shim-1")
	got := a.recv()
	if got.GetTask().GetId() != sent.GetTaskId() || got.GetSeq() != 1 {
		t.Errorf("after the reconnect: %v, want %s again with seq 1", got, sent.GetTaskId())
	}
}
