// Package mtls is how redoma-relay knows which agent is on a connection:
// from the client certificate Envoy presents for it, and from nothing else.
//
// Envoy's per-agent cluster (redoma-xds, agent_<repo>_relay) connects with
// that agent's X.509 SVID, spiffe://<td>/agent/<repo>. A connection is
// accepted only with an SVID that go-spiffe verifies against the trust
// domain's bundle (exactly one URI SAN, a leaf, a chain to the bundle), of
// the trust domain, whose path is /agent/<name> with a valid name that the
// allowlist (/etc/redoma/agents.txt) has. Anything else (no certificate,
// another trust domain, host/sds, host/relay, spire-server/upstream, a name
// not allowed) fails the handshake.
package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/bpalermo/redoma/common/allowlist"
	"github.com/bpalermo/redoma/relay/internal/svid"
)

// Allowed reports whether an agent of that name may connect.
type Allowed func(name string) (bool, error)

// ServerConfig is the relay's TLS: its own SVID from src, and an agent's
// SVID required of every client (Authorizer). TLS 1.3 only: the one peer is
// Envoy, configured for it.
func ServerConfig(src svid.Source, trustDomain string, allowed Allowed) (*tls.Config, error) {
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return nil, err
	}
	c := tlsconfig.MTLSServerConfig(src, src, Authorizer(td, allowed))
	c.MinVersion = tls.VersionTLS13
	return c, nil
}

// Authorizer accepts an SVID of td whose path is /agent/<name>, with a
// valid name the allowlist has.
func Authorizer(td spiffeid.TrustDomain, allowed Allowed) tlsconfig.Authorizer {
	return func(id spiffeid.ID, _ [][]*x509.Certificate) error {
		name, err := AgentName(id, td)
		if err != nil {
			return err
		}
		ok, err := allowed(name)
		if err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
		if !ok {
			return fmt.Errorf("agent %q is not in the allowlist", name)
		}
		return nil
	}
}

// AgentName is the agent an ID is: spiffe://<td>/agent/<name>, one segment,
// a valid name.
func AgentName(id spiffeid.ID, td spiffeid.TrustDomain) (string, error) {
	if id.TrustDomain() != td {
		return "", fmt.Errorf("%s is not of the trust domain %s", id, td)
	}
	name, ok := strings.CutPrefix(id.Path(), "/agent/")
	if !ok || !allowlist.ValidName(name) {
		return "", fmt.Errorf("%s is not an agent's ID (spiffe://%s/agent/<name>)", id, td)
	}
	return name, nil
}

// PeerAgent is the agent of a gRPC call on a connection ServerConfig
// accepted.
func PeerAgent(ctx context.Context, trustDomain string) (string, error) {
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return "", err
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("no peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return "", errors.New("the connection has no client certificate")
	}
	id, err := x509svid.IDFromCert(info.State.PeerCertificates[0])
	if err != nil {
		return "", err
	}
	return AgentName(id, td)
}
