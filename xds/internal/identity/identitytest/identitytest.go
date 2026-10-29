// Package identitytest is a fake of SPIRE's Delegated Identity API, in memory
// or on a unix socket, with a throwaway CA, for tests that need no real SPIRE.
package identitytest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/bpalermo/redoma/common/sockettest"
)

// ListenUnix is sockettest.ListenUnix.
func ListenUnix(t testing.TB, name string) (net.Listener, string) {
	t.Helper()
	return sockettest.ListenUnix(t, name)
}

// UnixSocketsRefused is sockettest.UnixSocketsRefused.
func UnixSocketsRefused(err error) bool { return sockettest.UnixSocketsRefused(err) }

// SkipIfRefused is sockettest.SkipIfRefused.
func SkipIfRefused(t testing.TB, err error) {
	t.Helper()
	sockettest.SkipIfRefused(t, err)
}

// CA issues SVIDs of one trust domain.
type CA struct {
	TrustDomain string
	cert        *x509.Certificate
	key         *ecdsa.PrivateKey
}

// NewCA returns a self-signed CA for trustDomain.
func NewCA(t testing.TB, trustDomain string) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA " + trustDomain},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		URIs:                  []*url.URL{{Scheme: "spiffe", Host: trustDomain}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{TrustDomain: trustDomain, cert: cert, key: key}
}

// DER is the CA's certificate.
func (ca *CA) DER() []byte { return ca.cert.Raw }

// SVID issues an X.509 SVID for path (such as "/agent/a") with its key.
func (ca *CA) SVID(t testing.TB, path string) *delegatedidentityv1.X509SVIDWithKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notAfter := time.Now().Add(time.Hour).Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		URIs:         []*url.URL{{Scheme: "spiffe", Host: ca.TrustDomain, Path: path}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &delegatedidentityv1.X509SVIDWithKey{
		X509Svid: &types.X509SVID{
			Id:        &types.SPIFFEID{TrustDomain: ca.TrustDomain, Path: path},
			CertChain: [][]byte{der},
			ExpiresAt: notAfter.Unix(),
		},
		X509SvidKey: pkcs8,
	}
}

// Server is the fake API. SVIDs are registered per selector, as SPIRE's
// one-selector entries are, and match as SPIRE matches: a request gets the
// SVIDs of every entry whose selector is among the request's, again on
// every change.
type Server struct {
	delegatedidentityv1.UnimplementedDelegatedIdentityServer

	mu      sync.Mutex
	svids   map[string][]*delegatedidentityv1.X509SVIDWithKey // by "type:value"
	bundles map[string][]byte
	changed chan struct{} // closed and replaced on every change
	streams map[string]int
	opened  map[string]int
	fail    chan struct{} // closed and replaced by FailStreams
}

// Serve starts a fake on an in-memory listener and returns it with a client
// connection to it. Both stop with the test.
func Serve(t testing.TB) (*Server, *grpc.ClientConn) {
	t.Helper()
	l := bufconn.Listen(1 << 20)
	s := ServeOn(t, l)
	conn, err := grpc.NewClient("passthrough:///delegated-identity",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return s, conn
}

// ServeOn starts a fake on l. It stops with the test.
func ServeOn(t testing.TB, l net.Listener) *Server {
	t.Helper()
	s := &Server{
		svids:   map[string][]*delegatedidentityv1.X509SVIDWithKey{},
		bundles: map[string][]byte{},
		changed: make(chan struct{}),
		streams: map[string]int{},
		opened:  map[string]int{},
		fail:    make(chan struct{}),
	}
	g := grpc.NewServer()
	delegatedidentityv1.RegisterDelegatedIdentityServer(g, s)
	go func() { _ = g.Serve(l) }()
	t.Cleanup(g.Stop)
	return s
}

func key(sel *types.Selector) string { return sel.GetType() + ":" + sel.GetValue() }

// keys are a request's selectors, type:value, without duplicates.
func keys(sels []*types.Selector) []string {
	var out []string
	for _, sel := range sels {
		if k := key(sel); !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// matching is the SVIDs of the entries whose selector is among sels; called
// with mu held.
func (s *Server) matching(sels []string) []*delegatedidentityv1.X509SVIDWithKey {
	var out []*delegatedidentityv1.X509SVIDWithKey
	for _, sel := range sels {
		out = append(out, s.svids[sel]...)
	}
	return out
}

// notify wakes every stream; called with mu held.
func (s *Server) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// SetSVIDs registers the SVIDs a subscription with selector type:value gets;
// none removes them.
func (s *Server) SetSVIDs(selector string, svids ...*delegatedidentityv1.X509SVIDWithKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.svids[selector] = svids
	s.notify()
}

// SetBundle sets the CA certificates (DER, concatenated) of the map key td.
func (s *Server) SetBundle(td string, der []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundles[td] = der
	s.notify()
}

// FailStreams ends every open stream with Unavailable, as an agent restart
// would.
func (s *Server) FailStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.fail)
	s.fail = make(chan struct{})
}

// Streams is how many SVID subscriptions with selector type:value are open.
func (s *Server) Streams(selector string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[selector]
}

// Opened is how many SVID subscriptions with selector type:value were ever
// opened.
func (s *Server) Opened(selector string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened[selector]
}

// checkSelectors refuses what SPIRE's handler refuses: both or neither of
// selectors and a PID.
func checkSelectors(sels []*types.Selector, pid int32) error {
	if (len(sels) == 0) == (pid == 0) {
		return status.Error(codes.InvalidArgument, "exactly one of selectors and pid")
	}
	return nil
}

func (s *Server) SubscribeToX509SVIDs(req *delegatedidentityv1.SubscribeToX509SVIDsRequest, stream grpc.ServerStreamingServer[delegatedidentityv1.SubscribeToX509SVIDsResponse]) error {
	if err := checkSelectors(req.GetSelectors(), req.GetPid()); err != nil {
		return err
	}
	sels := keys(req.GetSelectors())
	s.mu.Lock()
	for _, sel := range sels {
		s.streams[sel]++
		s.opened[sel]++
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for _, sel := range sels {
			s.streams[sel]--
		}
		s.mu.Unlock()
	}()
	return s.loop(stream.Context(), func() error {
		return stream.Send(&delegatedidentityv1.SubscribeToX509SVIDsResponse{X509Svids: s.matching(sels)})
	})
}

func (s *Server) SubscribeToX509Bundles(_ *delegatedidentityv1.SubscribeToX509BundlesRequest, stream grpc.ServerStreamingServer[delegatedidentityv1.SubscribeToX509BundlesResponse]) error {
	return s.loop(stream.Context(), func() error {
		bundles := map[string][]byte{}
		for k, v := range s.bundles {
			bundles[k] = v
		}
		return stream.Send(&delegatedidentityv1.SubscribeToX509BundlesResponse{CaCertificates: bundles})
	})
}

// loop sends the current state with mu held, then again after every change,
// until the stream ends or FailStreams.
func (s *Server) loop(ctx context.Context, send func() error) error {
	for {
		s.mu.Lock()
		err := send()
		changed, fail := s.changed, s.fail
		s.mu.Unlock()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-fail:
			return status.Error(codes.Unavailable, "fake: streams failed")
		case <-changed:
		}
	}
}

// FetchJWTSVIDs answers with a token naming the SVID path and audiences of
// the selector's first registered X.509 SVID.
func (s *Server) FetchJWTSVIDs(_ context.Context, req *delegatedidentityv1.FetchJWTSVIDsRequest) (*delegatedidentityv1.FetchJWTSVIDsResponse, error) {
	if err := checkSelectors(req.GetSelectors(), req.GetPid()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	svids := s.matching(keys(req.GetSelectors()))
	if len(svids) == 0 {
		return &delegatedidentityv1.FetchJWTSVIDsResponse{}, nil
	}
	id := svids[0].GetX509Svid().GetId()
	return &delegatedidentityv1.FetchJWTSVIDsResponse{Svids: []*types.JWTSVID{{
		Token:     "jwt:" + id.GetPath() + ":" + strings.Join(req.GetAudience(), ","),
		Id:        id,
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
		IssuedAt:  time.Now().Unix(),
	}}}, nil
}
