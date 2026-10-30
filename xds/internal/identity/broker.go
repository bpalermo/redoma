package identity

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/bpalermo/redoma/xds/internal/agent"
)

// BrokerHeader is the gRPC metadata key every Broker API call carries, set
// to "true": the specification's guard against a request forged through
// another service (SPIRE refuses a call without it).
const BrokerHeader = "broker.spiffe.io"

// PIDReferenceType is the type URL of a WorkloadPIDReference, the one
// reference type redoma uses and SPIRE's broker block must allow it.
const PIDReferenceType = "type.googleapis.com/spiffe.broker.WorkloadPIDReference"

// SPIREAgentPath is the path prefix of a SPIRE agent's SPIFFE ID: the
// Broker API's server must present one.
const SPIREAgentPath = "/spire/agent/"

// BundleWatcher gives the trust domain's X.509 bundle, PEM, each time it
// changes, until ctx ends (Workload: redoma xds's own Workload API stream).
type BundleWatcher interface {
	WatchX509Bundle(ctx context.Context, update func([]byte))
}

// SVIDSource is what redoma xds presents to the Broker API, and how it
// checks the SPIRE agent's certificate: its own SVID and the trust
// domain's bundle (Workload).
type SVIDSource interface {
	x509svid.Source
	x509bundle.Source
}

// DialBroker connects to the Broker API on the unix socket socketPath, with
// mutual TLS: it presents src's SVID and accepts a SPIRE agent of
// trustDomain. Every call carries BrokerHeader.
func DialBroker(socketPath string, src SVIDSource, trustDomain spiffeid.TrustDomain) (*grpc.ClientConn, error) {
	creds := grpccredentials.MTLSClientCredentials(src, src, SPIREAgentAuthorizer(trustDomain))
	return grpc.NewClient("unix://"+socketPath, append(BrokerDialOptions(), grpc.WithTransportCredentials(creds))...)
}

// BrokerDialOptions are DialBroker's options but the transport's: the
// metadata every call must carry.
func BrokerDialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(withBrokerHeader(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(withBrokerHeader(ctx), desc, cc, method, opts...)
		}),
	}
}

func withBrokerHeader(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, BrokerHeader, "true")
}

// SPIREAgentAuthorizer accepts a SPIFFE ID of trustDomain under
// SPIREAgentPath: the SPIRE agent serving the Broker API.
func SPIREAgentAuthorizer(trustDomain spiffeid.TrustDomain) tlsconfig.Authorizer {
	return tlsconfig.AdaptMatcher(func(id spiffeid.ID) error {
		if !id.MemberOf(trustDomain) || !strings.HasPrefix(id.Path(), SPIREAgentPath) {
			return fmt.Errorf("%s is not a SPIRE agent of %s", id, trustDomain)
		}
		return nil
	})
}

// Broker is redoma xds's view of the SPIFFE Broker API: each agent is named
// by its container's process (agent.Agent.Process), whose PID SPIRE's
// attestors turn into selectors. Every response is checked against the
// process: it is used only if the process was still alive once SPIRE had
// answered, so the PID SPIRE attested was its own (docs/broker-identity.md,
// "PID reuse"), and only for the agent's own SPIFFE ID.
type Broker struct {
	api         broker.APIClient
	bundles     BundleWatcher
	trustDomain string
	log         *slog.Logger

	// MinBackoff and MaxBackoff bound the wait before a stream is
	// resubscribed, as Client's do.
	MinBackoff, MaxBackoff time.Duration
}

var _ JWTFetcher = (*Broker)(nil)

// NewBroker returns a client of the Broker API on conn (DialBroker) for the
// agents of trustDomain, serving the bundle bundles gives.
func NewBroker(conn grpc.ClientConnInterface, bundles BundleWatcher, trustDomain string, log *slog.Logger) *Broker {
	return &Broker{
		api:         broker.NewAPIClient(conn),
		bundles:     bundles,
		trustDomain: trustDomain,
		log:         log,
		MinBackoff:  time.Second,
		MaxBackoff:  30 * time.Second,
	}
}

// reference names a's process for SPIRE. An agent without a process, or
// whose process has exited, is never subscribed again (errFinal).
func reference(a agent.Agent) (*broker.WorkloadReference, error) {
	if a.Process == nil {
		return nil, fmt.Errorf("%w: agent %s has no pidfd of its container's process (a launcher's agent, or one registered before redoma xds lost it): restart its container", errFinal, a.ID)
	}
	pid, err := a.Process.PID()
	if err != nil {
		return nil, fmt.Errorf("%w: agent %s's process: %w", errFinal, a.ID, err)
	}
	ref, err := anypb.New(&broker.WorkloadPIDReference{Pid: int32(pid)})
	if err != nil {
		return nil, err
	}
	return &broker.WorkloadReference{Reference: ref}, nil
}

// errExited is a's process found exited after SPIRE answered: what SPIRE
// attested may have been another process.
func errExited(a agent.Agent) error {
	return fmt.Errorf("%w: agent %s's process exited while SPIRE attested it; its answer is discarded", errFinal, a.ID)
}

// WatchX509SVID calls update with the agent's SVID each time SPIRE sends
// one, and with nil when SPIRE has none for it, until ctx ends or the
// agent's process is found gone. A stream that fails is resubscribed with
// backoff, attesting the process again.
func (b *Broker) WatchX509SVID(ctx context.Context, a agent.Agent, update func(*X509SVID)) {
	retry(ctx, b.log, b.MinBackoff, b.MaxBackoff, "x509-svid", slog.String("agent", a.ID), func(ctx context.Context) (bool, error) {
		return b.subscribe(ctx, a, update)
	})
}

func (b *Broker) subscribe(ctx context.Context, a agent.Agent, update func(*X509SVID)) (bool, error) {
	ref, err := reference(a)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := b.api.SubscribeToX509SVID(ctx, &broker.SubscribeToX509SVIDRequest{Reference: ref})
	if err != nil {
		return false, err
	}
	want := AgentID(b.trustDomain, a.ID)
	for received := false; ; received = true {
		resp, err := stream.Recv()
		if err != nil {
			return received, err
		}
		// SPIRE attests once, before its first answer: the process alive now
		// was alive then, and held the PID SPIRE read.
		if !received && !a.Process.Alive() {
			return false, errExited(a)
		}
		svid, err := pickBrokerSVID(resp.GetSvids(), want)
		if err != nil {
			b.log.Error("unusable SVID from SPIRE", "agent", a.ID, "err", err)
			continue
		}
		update(svid)
	}
}

// WatchX509Bundle is the bundle watcher's: the Broker API's bundle streams
// need a workload, and the bundle is served while no agent runs.
func (b *Broker) WatchX509Bundle(ctx context.Context, update func([]byte)) {
	b.bundles.WatchX509Bundle(ctx, update)
}

// FetchJWTSVID mints the agent's JWT SVID for audience. SPIRE attests the
// process again on every call, and returns a JWT-SVID for every entry its
// selectors match (it ignores the request's spiffe_id): only the agent's
// own is taken.
func (b *Broker) FetchJWTSVID(ctx context.Context, a agent.Agent, audience []string) (*JWTSVID, error) {
	if len(audience) == 0 {
		return nil, errors.New("a JWT SVID needs an audience")
	}
	ref, err := reference(a)
	if err != nil {
		return nil, err
	}
	want := AgentID(b.trustDomain, a.ID)
	resp, err := b.api.FetchJWTSVID(ctx, &broker.FetchJWTSVIDRequest{Reference: ref, Audience: audience, SpiffeId: want})
	if err != nil {
		return nil, err
	}
	if !a.Process.Alive() {
		return nil, errExited(a)
	}
	for _, s := range resp.GetSvids() {
		if s.GetSpiffeId() != want {
			continue
		}
		parsed, err := jwtsvid.ParseInsecure(s.GetSvid(), audience)
		if err != nil {
			return nil, fmt.Errorf("SPIRE's JWT SVID for %s: %w", want, err)
		}
		if parsed.ID.String() != want {
			return nil, fmt.Errorf("SPIRE's JWT SVID for %s names %s", want, parsed.ID)
		}
		return &JWTSVID{ID: want, Token: s.GetSvid(), ExpiresAt: parsed.Expiry}, nil
	}
	return nil, fmt.Errorf("SPIRE returned no JWT SVID for %s", want)
}

// pickBrokerSVID returns the SVID with ID want, PEM-encoded, or nil if there
// is none. Its leaf must name want too.
func pickBrokerSVID(svids []*broker.X509SVID, want string) (*X509SVID, error) {
	for _, s := range svids {
		if s.GetSpiffeId() != want {
			continue
		}
		certs, err := x509.ParseCertificates(s.GetX509Svid())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", want, err)
		}
		if len(certs) == 0 {
			return nil, fmt.Errorf("%s: empty certificate chain", want)
		}
		if len(certs[0].URIs) != 1 || certs[0].URIs[0].String() != want {
			return nil, fmt.Errorf("%s: the leaf certificate names %v", want, certs[0].URIs)
		}
		if _, err := x509.ParsePKCS8PrivateKey(s.GetX509SvidKey()); err != nil {
			// The error says nothing about the key's bytes.
			return nil, fmt.Errorf("%s: private key: %w", want, err)
		}
		var chain []byte
		for _, c := range certs {
			chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
		}
		return &X509SVID{
			ID:           want,
			CertChainPEM: chain,
			KeyPEM:       pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: s.GetX509SvidKey()}),
			ExpiresAt:    certs[0].NotAfter,
		}, nil
	}
	return nil, nil
}
