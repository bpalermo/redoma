package rolesanywhere

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"time"
)

// redoma xds has no network of its own (PrivateNetwork, AF_UNIX only): it
// reaches Roles Anywhere through the host Envoy, which serves a plain TCP
// tunnel to each endpoint in use, rolesanywhere.<region>.amazonaws.com:443,
// on a unix socket (lds.TunnelListener, cds.TunnelCluster). The TLS is
// redoma xds's own, end to end: Envoy carries only ciphertext.

// endpointHost is what a tunnel may lead to: a Roles Anywhere endpoint
// (Host), nothing else.
var endpointHost = regexp.MustCompile(`^rolesanywhere\.[a-z]{2}(?:-gov)?-[a-z]+-[0-9]\.amazonaws\.com(?:\.cn)?$`)

// IsEndpoint reports whether host is a Roles Anywhere endpoint.
func IsEndpoint(host string) bool {
	return endpointHost.MatchString(host)
}

// TunnelSocket is the socket of the tunnel to host, in dir.
func TunnelSocket(dir, host string) string {
	return filepath.Join(dir, host+".sock")
}

// Tunnels makes the HTTP client whose connections go through the tunnels.
type Tunnels struct {
	// Dir holds the tunnels' sockets (/run/redoma/xds/tunnel).
	Dir string
	// Roots verify the endpoint's certificate: the system's when nil. It is
	// always verified, for the endpoint's host name.
	Roots *x509.CertPool
	// Dial opens a stream to the socket at path; a unix socket when nil.
	Dial func(ctx context.Context, path string) (net.Conn, error)
}

// Client is the HTTP client: each connection to a Roles Anywhere endpoint
// is a dial of its tunnel's socket, then TLS 1.2 or later with the
// endpoint's name. Any other host, or a port other than 443, is refused
// before anything is dialled.
func (t Tunnels) Client() *http.Client {
	dial := t.Dial
	if dial == nil {
		dial = func(ctx context.Context, path string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		// A redirect is the answer, never followed: following a 307 or 308
		// would send the signed body and the certificate headers to wherever
		// it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if !IsEndpoint(host) || port != "443" {
					return nil, fmt.Errorf("%s is not a Roles Anywhere endpoint", addr)
				}
				return dial(ctx, TunnelSocket(t.Dir, host))
			},
			// Never a proxy from the environment: the tunnel is the way.
			Proxy:               nil,
			TLSClientConfig:     &tls.Config{RootCAs: t.Roots, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 2,
		},
	}
}
