package rolesanywhere

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// testCA is a throwaway CA standing in for the system roots.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// serverCert is a certificate for name, issued by the CA.
func (ca *testCA) serverCert(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// endpoint is a TLS server with cert, reached by the Tunnels' Dial, which
// records the socket paths it was asked for.
type endpoint struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newEndpoint(t *testing.T, cert tls.Certificate) *endpoint {
	t.Helper()
	e := &endpoint{srv: httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Host)
	}))}
	e.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	e.srv.StartTLS()
	t.Cleanup(e.srv.Close)
	return e
}

func (e *endpoint) dial(ctx context.Context, path string) (net.Conn, error) {
	e.mu.Lock()
	e.paths = append(e.paths, path)
	e.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, "tcp", e.srv.Listener.Addr().String())
}

func (e *endpoint) dialled() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paths
}

const host = "rolesanywhere.us-east-1.amazonaws.com"

// TestTunnel: a request to the endpoint dials its tunnel's socket and
// verifies the endpoint's certificate for its name against the roots.
func TestTunnel(t *testing.T) {
	ca := newTestCA(t)
	e := newEndpoint(t, ca.serverCert(t, host))
	c := Tunnels{Dir: "/run/redoma/xds/tunnel", Roots: ca.pool, Dial: e.dial}.Client()
	resp, err := c.Get("https://" + host + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != host {
		t.Errorf("the endpoint saw host %q", body)
	}
	if got := e.dialled(); len(got) != 1 || got[0] != "/run/redoma/xds/tunnel/"+host+".sock" {
		t.Errorf("dialled %q", got)
	}
}

// TestTunnelVerifies: a certificate for another name, or from a CA the
// roots do not hold, fails the handshake.
func TestTunnelVerifies(t *testing.T) {
	ca, stranger := newTestCA(t), newTestCA(t)
	for name, cert := range map[string]tls.Certificate{
		"another name":      ca.serverCert(t, "rolesanywhere.us-east-2.amazonaws.com"),
		"an unknown issuer": stranger.serverCert(t, host),
	} {
		e := newEndpoint(t, cert)
		c := Tunnels{Dir: "/run/redoma/xds/tunnel", Roots: ca.pool, Dial: e.dial}.Client()
		if resp, err := c.Get("https://" + host + "/sessions"); err == nil {
			resp.Body.Close()
			t.Errorf("%s: the handshake passed", name)
		}
	}
}

// TestTunnelOnlyToEndpoints: any host but a Roles Anywhere endpoint, or a
// port but 443, is refused before a socket is dialled.
func TestTunnelOnlyToEndpoints(t *testing.T) {
	ca := newTestCA(t)
	e := newEndpoint(t, ca.serverCert(t, host))
	c := Tunnels{Dir: "/run/redoma/xds/tunnel", Roots: ca.pool, Dial: e.dial}.Client()
	for _, u := range []string{
		"https://example.org/",
		"https://" + host + ":8443/",
		"https://rolesanywhere.us-east-1.amazonaws.com.example.org/",
		"https://sts.us-east-1.amazonaws.com/",
		"http://" + host + "/",
	} {
		if resp, err := c.Get(u); err == nil {
			resp.Body.Close()
			t.Errorf("%s: dialled", u)
		}
	}
	if got := e.dialled(); len(got) != 0 {
		t.Errorf("dialled %q", got)
	}
}

func TestIsEndpoint(t *testing.T) {
	for h, want := range map[string]bool{
		host: true,
		"rolesanywhere.us-gov-west-1.amazonaws.com":  true,
		"rolesanywhere.cn-north-1.amazonaws.com.cn":  true,
		"rolesanywhere.us-east-1.amazonaws.com.":     false,
		"rolesanywhere.us-east-1.amazonaws.com:443":  false,
		"rolesanywhere.us-east-1.amazonaws.co":       false,
		"Rolesanywhere.us-east-1.amazonaws.com":      false,
		"x.rolesanywhere.us-east-1.amazonaws.com":    false,
		"rolesanywhere.us-east-1.amazonaws.com/path": false,
	} {
		if IsEndpoint(h) != want {
			t.Errorf("IsEndpoint(%q) = %v", h, !want)
		}
	}
	if !IsEndpoint(Host("aws", "eu-west-3")) || !IsEndpoint(Host("aws-cn", "cn-northwest-1")) {
		t.Error("Host makes a host IsEndpoint refuses")
	}
}
