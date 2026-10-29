package mtls

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bpalermo/redoma/relay/internal/mtls/mtlstest"
	"github.com/bpalermo/redoma/relay/internal/svid"
)

const td = "redoma.internal"

func allowOnly(names ...string) Allowed {
	return func(name string) (bool, error) {
		for _, n := range names {
			if n == name {
				return true, nil
			}
		}
		return false, nil
	}
}

// handshake runs a TLS handshake between ServerConfig and a client with
// cert (none when nil), and returns the server's error.
func handshake(t *testing.T, src svid.Source, allowed Allowed, cert *tls.Certificate) error {
	t.Helper()
	cfg, err := ServerConfig(src, td, allowed)
	if err != nil {
		t.Fatal(err)
	}
	sc, cc := net.Pipe()
	defer sc.Close()
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}
	if cert != nil {
		client.Certificates = []tls.Certificate{*cert}
	}
	go func() {
		c := tls.Client(cc, client)
		_ = c.HandshakeContext(ctx)
		// With TLS 1.3 the client learns of a refusal on its first read.
		_, _ = c.Read(make([]byte, 1))
		c.Close()
	}()
	return tls.Server(sc, cfg).HandshakeContext(ctx)
}

// TestOnlyAllowedAgents: a connection is accepted with an SVID of the trust
// domain's CA naming an allowed agent, and refused otherwise, the other
// workloads of the trust domain included.
func TestOnlyAllowedAgents(t *testing.T) {
	ca := mtlstest.NewCA(t, td)
	src := ca.Source(t, "spiffe://"+td+"/host/relay")
	allowed := allowOnly("beta", "sds", "relay")
	other := mtlstest.NewCA(t, td)

	good := ca.Issue(t, "spiffe://"+td+"/agent/beta")
	if err := handshake(t, src, allowed, &good); err != nil {
		t.Fatalf("an allowed agent's SVID: %v", err)
	}
	for name, tc := range map[string]struct {
		cert *tls.Certificate
		want string
	}{
		"no client certificate":  {nil, "certificate"},
		"another trust domain":   {ptr(ca.Issue(t, "spiffe://example.org/agent/beta")), ""},
		"host/sds":               {ptr(ca.Issue(t, "spiffe://"+td+"/host/sds")), "not an agent's ID"},
		"host/relay":             {ptr(ca.Issue(t, "spiffe://"+td+"/host/relay")), "not an agent's ID"},
		"spire-server/upstream":  {ptr(ca.Issue(t, "spiffe://"+td+"/spire-server/upstream")), "not an agent's ID"},
		"a deeper path":          {ptr(ca.Issue(t, "spiffe://"+td+"/agent/beta/x")), "not an agent's ID"},
		"not a name":             {ptr(ca.Issue(t, "spiffe://"+td+"/agent/Beta")), "not an agent's ID"},
		"not in the allowlist":   {ptr(ca.Issue(t, "spiffe://"+td+"/agent/alpha")), "not in the allowlist"},
		"another CA, same td":    {ptr(other.Issue(t, "spiffe://"+td+"/agent/beta")), ""},
		"two IDs":                {ptr(ca.Issue(t, "spiffe://"+td+"/agent/beta", "spiffe://"+td+"/agent/alpha")), ""},
		"no ID":                  {ptr(ca.Issue(t)), ""},
		"a query":                {ptr(ca.Issue(t, "spiffe://"+td+"/agent/beta?x=1")), ""},
		"a port":                 {ptr(ca.Issue(t, "spiffe://"+td+":443/agent/beta")), ""},
		"an agent of another td": {ptr(other.Issue(t, "spiffe://other.example.org/agent/beta")), ""},
	} {
		err := handshake(t, src, allowed, tc.cert)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want refused (%q)", name, err, tc.want)
		}
	}
}

// TestAllowlistErrorRefuses: an allowlist that cannot be read refuses every
// agent.
func TestAllowlistErrorRefuses(t *testing.T) {
	ca := mtlstest.NewCA(t, td)
	good := ca.Issue(t, "spiffe://"+td+"/agent/beta")
	broken := func(string) (bool, error) { return false, errors.New("unreadable") }
	if err := handshake(t, ca.Source(t, "spiffe://"+td+"/host/relay"), broken, &good); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("an unreadable allowlist: %v, want refused", err)
	}
}

func ptr[T any](v T) *T { return &v }
