package identity

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"testing"
	"time"

	"github.com/bpalermo/redoma/xds/internal/agent"
	"github.com/bpalermo/redoma/xds/internal/identity/identitytest"
)

const td = "redoma.internal"

// ag is an agent subscribed by the selector the fake registers SVIDs under.
// The client sends whatever selectors the agent carries; this format is the
// test's, not the client's.
func ag(id string) agent.Agent {
	return agent.Agent{
		ID:        id,
		Netns:     "/run/netns/redoma-" + id,
		Selectors: []agent.Selector{{Type: "redoma", Value: "agent:" + id}},
	}
}

func client(t *testing.T) (*Client, *identitytest.Server) {
	t.Helper()
	fake, conn := identitytest.Serve(t)
	c := New(conn, td, slog.New(slog.DiscardHandler))
	c.MinBackoff, c.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond
	return c, fake
}

// next returns the next value sent on ch, or fails the test after a while.
func next[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an update")
		panic("unreachable")
	}
}

func leafURI(t *testing.T, chainPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(chainPEM)
	if block == nil {
		t.Fatal("no PEM in the chain")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert.URIs[0].String()
}

func TestWatchX509SVID(t *testing.T) {
	c, fake := client(t)
	ca := identitytest.NewCA(t, td)
	sel := "redoma:agent:beta"
	// SPIRE may send other entries' SVIDs matching a subset of the selectors;
	// only the agent's own is used.
	fake.SetSVIDs(sel, ca.SVID(t, "/agent/other"), ca.SVID(t, "/agent/beta"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	updates := make(chan *X509SVID, 10)
	go c.WatchX509SVID(ctx, ag("beta"), func(s *X509SVID) { updates <- s })

	got := next(t, updates)
	if got == nil {
		t.Fatal("no SVID")
	}
	if got.ID != "spiffe://redoma.internal/agent/beta" {
		t.Errorf("ID = %q", got.ID)
	}
	if uri := leafURI(t, got.CertChainPEM); uri != got.ID {
		t.Errorf("leaf URI = %q, want %q", uri, got.ID)
	}
	if block, _ := pem.Decode(got.KeyPEM); block == nil || block.Type != "PRIVATE KEY" {
		t.Error("the key is not a PKCS#8 PEM block")
	}
	if time.Until(got.ExpiresAt) < 50*time.Minute {
		t.Errorf("ExpiresAt = %v", got.ExpiresAt)
	}

	// The entry goes away: no SVID.
	fake.SetSVIDs(sel)
	if got := next(t, updates); got != nil {
		t.Errorf("after the entry is gone, update(%v), want nil", got.ID)
	}
}

// TestWatchX509SVIDResubscribes: when the agent's stream ends (an agent
// restart), the client subscribes again and keeps delivering.
func TestWatchX509SVIDResubscribes(t *testing.T) {
	c, fake := client(t)
	ca := identitytest.NewCA(t, td)
	sel := "redoma:agent:a"
	fake.SetSVIDs(sel, ca.SVID(t, "/agent/a"))

	ctx, cancel := context.WithCancel(t.Context())
	updates := make(chan *X509SVID, 10)
	done := make(chan struct{})
	go func() {
		c.WatchX509SVID(ctx, ag("a"), func(s *X509SVID) { updates <- s })
		close(done)
	}()
	first := next(t, updates)

	fake.FailStreams()
	second := next(t, updates)
	if second == nil || string(second.CertChainPEM) != string(first.CertChainPEM) {
		t.Error("after resubscribing, the same SVID should be delivered again")
	}

	// Cancelling ends the watch and closes the stream.
	cancel()
	next(t, done)
	deadline := time.Now().Add(10 * time.Second)
	for fake.Streams(sel) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d stream(s) still open after cancel", fake.Streams(sel))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWatchX509Bundle(t *testing.T) {
	c, fake := client(t)
	ca := identitytest.NewCA(t, td)
	other := identitytest.NewCA(t, "other.example")
	fake.SetBundle("spiffe://other.example", other.DER())
	fake.SetBundle("spiffe://"+td, ca.DER())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	updates := make(chan []byte, 10)
	go c.WatchX509Bundle(ctx, func(b []byte) { updates <- b })

	// The first message may predate the second SetBundle; wait for ours.
	for {
		got := next(t, updates)
		if got == nil {
			continue
		}
		block, rest := pem.Decode(got)
		if block == nil || len(rest) != 0 {
			t.Fatalf("bundle is not one PEM certificate: %q", got)
		}
		if string(block.Bytes) != string(ca.DER()) {
			t.Fatal("bundle is not the trust domain's CA")
		}
		return
	}
}

// TestDialUnixSocket: Dial reaches the API on a unix socket, as on the admin
// socket.
func TestDialUnixSocket(t *testing.T) {
	l, path := identitytest.ListenUnix(t, "admin.sock")
	fake := identitytest.ServeOn(t, l)
	ca := identitytest.NewCA(t, td)
	fake.SetSVIDs("redoma:agent:a", ca.SVID(t, "/agent/a"))
	conn, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := New(conn, td, slog.New(slog.DiscardHandler)).FetchJWTSVID(t.Context(), ag("a"), []string{"x"}); err != nil {
		t.Fatal(err)
	}
}

func TestFetchJWTSVID(t *testing.T) {
	c, fake := client(t)
	ca := identitytest.NewCA(t, td)
	fake.SetSVIDs("redoma:agent:a", ca.SVID(t, "/agent/a"))

	got, err := c.FetchJWTSVID(t.Context(), ag("a"), []string{"sts.amazonaws.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "spiffe://redoma.internal/agent/a" || got.Token != "jwt:/agent/a:sts.amazonaws.com" {
		t.Errorf("got %+v", got)
	}

	if _, err := c.FetchJWTSVID(t.Context(), ag("nobody"), []string{"x"}); err == nil {
		t.Error("an agent without an entry got a JWT SVID")
	}
	if _, err := c.FetchJWTSVID(t.Context(), ag("a"), nil); err == nil {
		t.Error("a JWT SVID without an audience was fetched")
	}
}
