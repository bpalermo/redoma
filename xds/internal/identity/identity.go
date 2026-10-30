// Package identity fetches the agents' SPIFFE identities from SPIRE, as the
// one process SPIRE lets obtain them on the agents' behalf (uid 961,
// redoma-sds), through one of two APIs:
//
//   - Client, the Delegated Identity API on the SPIRE agent's admin socket.
//     An agent's identity is subscribed by the selectors its agent.Source
//     gives, which SPIRE takes as attested because the delegate sent them:
//     which namespace is which agent is decided by the Source and the
//     listeners, not by SPIRE.
//   - Broker, the SPIFFE Broker API (docs/broker-identity.md). An agent is
//     named by its container's process, whose pidfd the Source holds, and
//     SPIRE's attestors decide its selectors.
//
// An agent never talks to SPIRE.
package identity

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/bpalermo/redoma/xds/internal/agent"
)

// selectors converts an agent's selectors for the API.
func selectors(a agent.Agent) []*types.Selector {
	out := make([]*types.Selector, len(a.Selectors))
	for i, s := range a.Selectors {
		out[i] = &types.Selector{Type: s.Type, Value: s.Value}
	}
	return out
}

// AgentID is the SPIFFE ID of an agent.
func AgentID(trustDomain, repo string) string {
	return "spiffe://" + trustDomain + "/agent/" + repo
}

// X509SVID is an agent's X.509 SVID, PEM-encoded the way Envoy takes it.
type X509SVID struct {
	ID string
	// CertChainPEM is the leaf first, then any intermediates.
	CertChainPEM []byte
	// KeyPEM is the PKCS#8 private key. Never logged.
	KeyPEM    []byte
	ExpiresAt time.Time
}

// JWTSVID is an agent's JWT SVID for one audience.
type JWTSVID struct {
	ID        string
	Token     string
	ExpiresAt time.Time
}

// JWTFetcher mints JWT SVIDs for agents. Nothing serves them to Envoy yet;
// it is the interface the per-agent token broker will consume.
type JWTFetcher interface {
	FetchJWTSVID(ctx context.Context, a agent.Agent, audience []string) (*JWTSVID, error)
}

// Dial connects to the Delegated Identity API on the admin socket. gRPC
// redials on its own; a stream that ends is resubscribed by the Watch
// methods.
func Dial(socketPath string) (*grpc.ClientConn, error) {
	// The admin socket is a local unix socket, authenticated by the agent from
	// the caller's uid: there is no TLS to do.
	return grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// Client is the delegate's view of the Delegated Identity API.
type Client struct {
	api         delegatedidentityv1.DelegatedIdentityClient
	trustDomain string
	log         *slog.Logger

	// MinBackoff and MaxBackoff bound the wait before a stream is
	// resubscribed: it doubles from MinBackoff on each failure in a row, and
	// a stream that delivered anything starts over at MinBackoff.
	MinBackoff, MaxBackoff time.Duration
}

var _ JWTFetcher = (*Client)(nil)

// New returns a client of the API on conn for the agents of trustDomain.
func New(conn grpc.ClientConnInterface, trustDomain string, log *slog.Logger) *Client {
	return &Client{
		api:         delegatedidentityv1.NewDelegatedIdentityClient(conn),
		trustDomain: trustDomain,
		log:         log,
		MinBackoff:  time.Second,
		MaxBackoff:  30 * time.Second,
	}
}

// WatchX509SVID calls update with the agent's SVID each time SPIRE sends one, and
// with nil when SPIRE has none for it (no entry, or the entry was deleted),
// until ctx ends. A stream that fails is resubscribed with backoff; the last
// SVID stays current meanwhile, since it is valid until it expires.
func (c *Client) WatchX509SVID(ctx context.Context, a agent.Agent, update func(*X509SVID)) {
	want := AgentID(c.trustDomain, a.ID)
	retry(ctx, c.log, c.MinBackoff, c.MaxBackoff, "x509-svid", slog.String("agent", a.ID), func(ctx context.Context) (bool, error) {
		stream, err := c.api.SubscribeToX509SVIDs(ctx, &delegatedidentityv1.SubscribeToX509SVIDsRequest{Selectors: selectors(a)})
		if err != nil {
			return false, err
		}
		for received := false; ; received = true {
			resp, err := stream.Recv()
			if err != nil {
				return received, err
			}
			svid, err := pickSVID(resp.GetX509Svids(), want)
			if err != nil {
				c.log.Error("unusable SVID from SPIRE", "agent", a.ID, "err", err)
				continue
			}
			update(svid)
		}
	})
}

// WatchX509Bundle calls update with the trust domain's X.509 bundle, PEM, each
// time SPIRE sends the bundles, and with nil if the trust domain's is
// missing, until ctx ends. Other trust domains' bundles are ignored.
func (c *Client) WatchX509Bundle(ctx context.Context, update func([]byte)) {
	retry(ctx, c.log, c.MinBackoff, c.MaxBackoff, "x509-bundle", slog.String("trust_domain", c.trustDomain), func(ctx context.Context) (bool, error) {
		stream, err := c.api.SubscribeToX509Bundles(ctx, &delegatedidentityv1.SubscribeToX509BundlesRequest{})
		if err != nil {
			return false, err
		}
		for received := false; ; received = true {
			resp, err := stream.Recv()
			if err != nil {
				return received, err
			}
			bundle, err := c.pickBundle(resp.GetCaCertificates())
			if err != nil {
				c.log.Error("unusable bundle from SPIRE", "trust_domain", c.trustDomain, "err", err)
				continue
			}
			update(bundle)
		}
	})
}

// FetchJWTSVID mints the agent's JWT SVID for audience.
func (c *Client) FetchJWTSVID(ctx context.Context, a agent.Agent, audience []string) (*JWTSVID, error) {
	if len(audience) == 0 {
		return nil, errors.New("a JWT SVID needs an audience")
	}
	resp, err := c.api.FetchJWTSVIDs(ctx, &delegatedidentityv1.FetchJWTSVIDsRequest{Audience: audience, Selectors: selectors(a)})
	if err != nil {
		return nil, err
	}
	want := AgentID(c.trustDomain, a.ID)
	for _, s := range resp.GetSvids() {
		if idString(s.GetId()) == want {
			return &JWTSVID{ID: want, Token: s.GetToken(), ExpiresAt: time.Unix(s.GetExpiresAt(), 0)}, nil
		}
	}
	return nil, fmt.Errorf("SPIRE returned no JWT SVID for %s", want)
}

// errFinal ends a subscription for good: retry does not run it again.
var errFinal = errors.New("not resubscribed")

// retry runs a subscription until ctx ends, or it returns an error that
// wraps errFinal, waiting with jittered exponential backoff between
// attempts: from minWait, doubling on each failure in a row, up to maxWait,
// and from minWait again after an attempt that delivered anything.
func retry(ctx context.Context, log *slog.Logger, minWait, maxWait time.Duration, what string, attr slog.Attr, run func(context.Context) (received bool, err error)) {
	delay := minWait
	for {
		received, err := run(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errFinal) {
			log.Warn("SPIRE stream ended; not resubscribing", "stream", what, attr, "err", err)
			return
		}
		if received {
			delay = minWait
		}
		// Up to a quarter less, so subscriptions that failed together do not
		// all come back at once.
		wait := delay - time.Duration(rand.Int64N(int64(delay)/4+1))
		log.Warn("SPIRE stream ended; resubscribing", "stream", what, attr, "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		delay = min(2*delay, maxWait)
	}
}

func idString(id *types.SPIFFEID) string {
	if id == nil {
		return ""
	}
	return "spiffe://" + id.GetTrustDomain() + id.GetPath()
}

// pickSVID returns the SVID with ID want, PEM-encoded, or nil if there is none.
func pickSVID(svids []*delegatedidentityv1.X509SVIDWithKey, want string) (*X509SVID, error) {
	for _, s := range svids {
		if idString(s.GetX509Svid().GetId()) != want {
			continue
		}
		chain := s.GetX509Svid().GetCertChain()
		if len(chain) == 0 {
			return nil, fmt.Errorf("%s: empty certificate chain", want)
		}
		var certs []byte
		for _, der := range chain {
			if _, err := x509.ParseCertificate(der); err != nil {
				return nil, fmt.Errorf("%s: %w", want, err)
			}
			certs = append(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
		}
		if _, err := x509.ParsePKCS8PrivateKey(s.GetX509SvidKey()); err != nil {
			// The error says nothing about the key's bytes.
			return nil, fmt.Errorf("%s: private key: %w", want, err)
		}
		return &X509SVID{
			ID:           want,
			CertChainPEM: certs,
			KeyPEM:       pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: s.GetX509SvidKey()}),
			ExpiresAt:    time.Unix(s.GetX509Svid().GetExpiresAt(), 0),
		}, nil
	}
	return nil, nil
}

// pickBundle returns the trust domain's CA certificates, PEM, or nil if the
// map has none. SPIRE keys the map by trust domain; both its name and its
// spiffe:// ID are accepted.
func (c *Client) pickBundle(bundles map[string][]byte) ([]byte, error) {
	for td, der := range bundles {
		if strings.TrimPrefix(td, "spiffe://") != c.trustDomain {
			continue
		}
		certs, err := x509.ParseCertificates(der)
		if err != nil {
			return nil, err
		}
		if len(certs) == 0 {
			return nil, errors.New("no CA certificates")
		}
		var out []byte
		for _, cert := range certs {
			out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
		}
		return out, nil
	}
	return nil, nil
}
