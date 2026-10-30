package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/identity/identitytest"
)

// fakeProcess is an agent's process whose life the test decides.
type fakeProcess struct {
	pid  int
	dead atomic.Bool
}

func (p *fakeProcess) PID() (int, error) {
	if p.dead.Load() {
		return 0, errors.New("exited")
	}
	return p.pid, nil
}

func (p *fakeProcess) Alive() bool { return !p.dead.Load() }

// fakeBroker is SPIRE's Broker API: it "attests" a PID to the SVIDs and
// tokens the test set for it, and refuses a call without BrokerHeader.
type fakeBroker struct {
	broker.UnimplementedAPIServer

	mu     sync.Mutex
	svids  map[int32][]*broker.X509SVID
	tokens map[int32][]*broker.JWTSVID
	opened map[int32]int
	// attest runs between the attestation and the answer: a test kills
	// the process there.
	attest func(pid int32)
}

func (f *fakeBroker) pid(ctx context.Context, ref *broker.WorkloadReference) (int32, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(BrokerHeader); len(v) != 1 || v[0] != "true" {
		return 0, status.Error(codes.InvalidArgument, "missing the broker.spiffe.io header")
	}
	if got := ref.GetReference().GetTypeUrl(); got != PIDReferenceType {
		return 0, status.Errorf(codes.PermissionDenied, "reference type %q", got)
	}
	var pidRef broker.WorkloadPIDReference
	if err := ref.GetReference().UnmarshalTo(&pidRef); err != nil {
		return 0, err
	}
	if f.attest != nil {
		f.attest(pidRef.GetPid())
	}
	return pidRef.GetPid(), nil
}

func (f *fakeBroker) SubscribeToX509SVID(req *broker.SubscribeToX509SVIDRequest, stream grpc.ServerStreamingServer[broker.SubscribeToX509SVIDResponse]) error {
	pid, err := f.pid(stream.Context(), req.GetReference())
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.opened[pid]++
	svids := f.svids[pid]
	f.mu.Unlock()
	if err := stream.Send(&broker.SubscribeToX509SVIDResponse{Svids: svids}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

func (f *fakeBroker) FetchJWTSVID(ctx context.Context, req *broker.FetchJWTSVIDRequest) (*broker.FetchJWTSVIDResponse, error) {
	pid, err := f.pid(ctx, req.GetReference())
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &broker.FetchJWTSVIDResponse{Svids: f.tokens[pid]}, nil
}

func (f *fakeBroker) streams(pid int32) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened[pid]
}

// fakeBundles serves one bundle.
type fakeBundles []byte

func (b fakeBundles) WatchX509Bundle(ctx context.Context, update func([]byte)) {
	update(b)
	<-ctx.Done()
}

func newBroker(t *testing.T) (*Broker, *fakeBroker) {
	t.Helper()
	l := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeBroker{svids: map[int32][]*broker.X509SVID{}, tokens: map[int32][]*broker.JWTSVID{}, opened: map[int32]int{}}
	broker.RegisterAPIServer(srv, fake)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	opts := append(BrokerDialOptions(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }))
	conn, err := grpc.NewClient("passthrough:///broker", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	b := NewBroker(conn, fakeBundles("bundle"), "example.org", slog.New(slog.DiscardHandler))
	b.MinBackoff, b.MaxBackoff = time.Millisecond, 5*time.Millisecond
	return b, fake
}

// brokerSVID is ca's SVID for path, as the Broker API sends it.
func brokerSVID(t *testing.T, ca *identitytest.CA, id, path string) *broker.X509SVID {
	t.Helper()
	s := ca.SVID(t, path)
	return &broker.X509SVID{SpiffeId: id, X509Svid: s.GetX509Svid().GetCertChain()[0], X509SvidKey: s.GetX509SvidKey(), Bundle: ca.DER()}
}

// TestBrokerWatchX509SVID: the agent's process is named by its PID, with
// the header; of the SVIDs its attestation gives, the agent's own is kept.
func TestBrokerWatchX509SVID(t *testing.T) {
	b, fake := newBroker(t)
	ca := identitytest.NewCA(t, "example.org")
	fake.svids[4242] = []*broker.X509SVID{
		brokerSVID(t, ca, "spiffe://example.org/agent/other", "/agent/other"),
		brokerSVID(t, ca, "spiffe://example.org/agent/beta", "/agent/beta"),
	}
	a := agent.Agent{ID: "beta", Process: &fakeProcess{pid: 4242}}
	got := make(chan *X509SVID, 4)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go b.WatchX509SVID(ctx, a, func(s *X509SVID) { got <- s })
	s := next(t, got)
	if s == nil || s.ID != "spiffe://example.org/agent/beta" || leafURI(t, s.CertChainPEM) != s.ID {
		t.Fatalf("SVID %+v, want beta's", s)
	}
}

// TestBrokerDiscardsAnAnswerAfterTheProcessExited: the process exited
// while SPIRE attested its PID, which another process may then have had:
// the answer is not used, and the agent is not subscribed again.
func TestBrokerDiscardsAnAnswerAfterTheProcessExited(t *testing.T) {
	b, fake := newBroker(t)
	ca := identitytest.NewCA(t, "example.org")
	fake.svids[4242] = []*broker.X509SVID{brokerSVID(t, ca, "spiffe://example.org/agent/beta", "/agent/beta")}
	proc := &fakeProcess{pid: 4242}
	fake.attest = func(int32) { proc.dead.Store(true) }
	got := make(chan *X509SVID, 4)
	done := make(chan struct{})
	go func() {
		b.WatchX509SVID(t.Context(), agent.Agent{ID: "beta", Process: proc}, func(s *X509SVID) { got <- s })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WatchX509SVID kept going after the process exited")
	}
	select {
	case s := <-got:
		t.Fatalf("an SVID from an attestation of an exited process: %+v", s)
	default:
	}
	if n := fake.streams(4242); n != 1 {
		t.Errorf("%d streams, want 1 and no resubscription", n)
	}
}

// TestBrokerWithoutAProcess: an agent with no pidfd (a launcher's, or one
// whose pidfd was lost) is never named to SPIRE.
func TestBrokerWithoutAProcess(t *testing.T) {
	b, fake := newBroker(t)
	done := make(chan struct{})
	go func() {
		b.WatchX509SVID(t.Context(), agent.Agent{ID: "beta"}, func(*X509SVID) { t.Error("an SVID for an agent without a process") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("WatchX509SVID kept going without a process")
	}
	if _, err := b.FetchJWTSVID(t.Context(), agent.Agent{ID: "beta"}, []string{"aud"}); err == nil {
		t.Error("FetchJWTSVID without a process: no error")
	}
	if len(fake.opened) != 0 {
		t.Errorf("streams %v, want none", fake.opened)
	}
}

// token is an unsigned JWT for sub and aud, expiring at exp, as
// jwtsvid.ParseInsecure reads one.
func token(t *testing.T, sub string, aud []string, exp time.Time) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "ES256", "typ": "JWT", "kid": "k"}) + "." +
		enc(map[string]any{"sub": sub, "aud": aud, "exp": exp.Unix()}) + "." +
		base64.RawURLEncoding.EncodeToString(make([]byte, 64))
}

// TestBrokerFetchJWTSVID: SPIRE returns a token for every entry the
// selectors match, whatever spiffe_id asked: only the agent's is taken,
// and nothing once its process has exited.
func TestBrokerFetchJWTSVID(t *testing.T) {
	b, fake := newBroker(t)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	aud := []string{"https://api.example.org"}
	fake.tokens[4242] = []*broker.JWTSVID{
		{SpiffeId: "spiffe://example.org/uid/1000", Svid: token(t, "spiffe://example.org/uid/1000", aud, exp)},
		{SpiffeId: "spiffe://example.org/agent/beta", Svid: token(t, "spiffe://example.org/agent/beta", aud, exp)},
	}
	proc := &fakeProcess{pid: 4242}
	a := agent.Agent{ID: "beta", Process: proc}
	got, err := b.FetchJWTSVID(t.Context(), a, aud)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "spiffe://example.org/agent/beta" || !got.ExpiresAt.Equal(exp) {
		t.Errorf("JWT SVID %s expiring %v, want beta's expiring %v", got.ID, got.ExpiresAt, exp)
	}

	// A token whose subject is not the ID it is listed under is refused.
	fake.tokens[4242] = []*broker.JWTSVID{{SpiffeId: "spiffe://example.org/agent/beta", Svid: token(t, "spiffe://example.org/agent/other", aud, exp)}}
	if _, err := b.FetchJWTSVID(t.Context(), a, aud); err == nil || !strings.Contains(err.Error(), "names") {
		t.Errorf("a token naming another ID: %v", err)
	}
	// None of the agent's.
	fake.tokens[4242] = fake.tokens[4242][:0]
	if _, err := b.FetchJWTSVID(t.Context(), a, aud); err == nil {
		t.Error("no token of the agent's: no error")
	}

	fake.tokens[4242] = []*broker.JWTSVID{{SpiffeId: "spiffe://example.org/agent/beta", Svid: token(t, "spiffe://example.org/agent/beta", aud, exp)}}
	fake.attest = func(int32) { proc.dead.Store(true) }
	if _, err := b.FetchJWTSVID(t.Context(), a, aud); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("a token from an attestation of an exited process: %v", err)
	}
	if _, err := b.FetchJWTSVID(t.Context(), a, nil); err == nil {
		t.Error("no audience: no error")
	}
}

// TestPickBrokerSVIDRefusesAMismatchedLeaf: the SVID listed under the
// agent's ID must name it.
func TestPickBrokerSVIDRefusesAMismatchedLeaf(t *testing.T) {
	ca := identitytest.NewCA(t, "example.org")
	s := brokerSVID(t, ca, "spiffe://example.org/agent/beta", "/agent/other")
	if _, err := pickBrokerSVID([]*broker.X509SVID{s}, "spiffe://example.org/agent/beta"); err == nil {
		t.Error("a leaf naming another agent was accepted")
	}
	if got, err := pickBrokerSVID(nil, "spiffe://example.org/agent/beta"); got != nil || err != nil {
		t.Errorf("no SVID: %v, %v", got, err)
	}
}

func TestSPIREAgentAuthorizer(t *testing.T) {
	td := spiffeid.RequireTrustDomainFromString("example.org")
	authorize := SPIREAgentAuthorizer(td)
	for id, ok := range map[string]bool{
		"spiffe://example.org/spire/agent/x509pop/abc": true,
		"spiffe://example.org/spire/agentx":            false,
		"spiffe://example.org/host/sds":                false,
		"spiffe://example.net/spire/agent/x509pop/abc": false,
	} {
		err := authorize(spiffeid.RequireFromString(id), nil)
		if (err == nil) != ok {
			t.Errorf("authorize(%s) = %v", id, err)
		}
	}
}

// TestBrokerWireContract pins what redoma relies on of go-spiffe's exp/
// bindings, which carry no compatibility promise: the reference's type URL,
// and the message and field names of the SPIFFE Broker API.
func TestBrokerWireContract(t *testing.T) {
	ref, err := anypb.New(&broker.WorkloadPIDReference{Pid: 7})
	if err != nil {
		t.Fatal(err)
	}
	if ref.GetTypeUrl() != PIDReferenceType {
		t.Errorf("type URL %q, want %q", ref.GetTypeUrl(), PIDReferenceType)
	}
	for msg, fields := range map[proto.Message][]string{
		&broker.WorkloadPIDReference{}:       {"pid"},
		&broker.WorkloadReference{}:          {"reference"},
		&broker.SubscribeToX509SVIDRequest{}: {"reference"},
		&broker.X509SVID{}:                   {"spiffe_id", "x509_svid", "x509_svid_key"},
		&broker.FetchJWTSVIDRequest{}:        {"reference", "audience", "spiffe_id"},
		&broker.JWTSVID{}:                    {"spiffe_id", "svid"},
	} {
		d := msg.ProtoReflect().Descriptor()
		if !strings.HasPrefix(string(d.FullName()), "spiffe.broker.") {
			t.Errorf("%s: not in package spiffe.broker", d.FullName())
		}
		for _, f := range fields {
			if d.Fields().ByName(protoreflect.Name(f)) == nil {
				t.Errorf("%s has no field %s", d.FullName(), f)
			}
		}
	}
	if BrokerHeader != "broker.spiffe.io" {
		t.Errorf("BrokerHeader = %q", BrokerHeader)
	}
}

// TestWatchBundle: the bundle is sent at once, and again only when an
// update changed it.
func TestWatchBundle(t *testing.T) {
	var mu sync.Mutex
	current := []byte("a")
	get := func() ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}
	updated := make(chan struct{})
	got := make(chan []byte, 8)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go watchBundle(ctx, get, updated, func(b []byte) { got <- b }, slog.New(slog.DiscardHandler))
	if b := next(t, got); string(b) != "a" {
		t.Fatalf("first bundle %q", b)
	}
	updated <- struct{}{} // unchanged
	mu.Lock()
	current = []byte("b")
	mu.Unlock()
	updated <- struct{}{}
	if b := next(t, got); string(b) != "b" {
		t.Errorf("second bundle %q, want b (the unchanged one is not sent)", b)
	}
}
