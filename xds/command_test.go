package xds

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bpalermo/redoma/common/config"
)

// TestCheckRelayID: only spiffe://<td>/host/relay, exactly as go-spiffe
// parses it, is the relay's ID; any other workload of the trust domain, one
// of another, and every spelling trick are refused.
func TestCheckRelayID(t *testing.T) {
	const td = "redoma.internal"
	if err := checkRelayID("spiffe://redoma.internal/host/relay", td); err != nil {
		t.Errorf("the relay's ID: %v", err)
	}
	for _, id := range []string{
		"spiffe://example.org/host/relay",
		"spiffe://other.example.org/host/relay",
		"spiffe://redoma.internal/agent/x",
		"spiffe://redoma.internal/host/sds",
		"spiffe://redoma.internal/spire-server/upstream",
		"spiffe://redoma.internal/host/relay/x",
		"spiffe://redoma.internal/host/relay/",
		"spiffe://redoma.internal/host",
		"spiffe://redoma.internal",
		"spiffe://Redoma.Internal/host/relay",
		"spiffe://redoma.internal/Host/Relay",
		"SPIFFE://redoma.internal/host/relay",
		"spiffe://redoma.internal./host/relay",
		"spiffe://user@redoma.internal/host/relay",
		"spiffe://redoma.internal:443/host/relay",
		"spiffe://redoma.internal/host/./relay",
		"spiffe://redoma.internal/host//relay",
		"spiffe://redoma.internal/host/relay?x=1",
		"spiffe://redoma.internal/host/relay#x",
		"spiffe://redoma.internal/host/%72elay",
		" spiffe://redoma.internal/host/relay",
		"",
	} {
		if err := checkRelayID(id, td); err == nil {
			t.Errorf("%q: accepted", id)
		}
	}
	if err := checkRelayID("spiffe://redoma.internal/host/relay", "not a domain/"); err == nil {
		t.Error("a bad trust domain: accepted")
	}
}

// TestSettle: the trust domain, the SPIRE admin socket and the relay's ID
// come from the settings file unless a flag gives them; the relay's ID
// follows the trust domain finally chosen.
func TestSettle(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "redoma.conf")
	if err := os.WriteFile(conf, []byte("trust_domain = example.org\nspire_admin_socket = /run/spire/admin.sock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args               []string
		td, admin, relayID string
	}{
		{nil, "redoma.internal", "/run/redoma-spire/admin/admin.sock", "spiffe://redoma.internal/host/relay"},
		{[]string{"--config", conf}, "example.org", "/run/spire/admin.sock", "spiffe://example.org/host/relay"},
		{[]string{"--config", conf, "--trust-domain", "example.net"}, "example.net", "/run/spire/admin.sock", "spiffe://example.net/host/relay"},
		{[]string{"--config", conf, "--relay-id", "spiffe://example.org/host/relay", "--spire-admin-socket", "/a.sock"}, "example.org", "/a.sock", "spiffe://example.org/host/relay"},
	} {
		var f flags
		cmd := command(&f)
		// The root command's persistent flag, as redoma has it.
		cmd.Flags().String(config.FlagName, filepath.Join(t.TempDir(), "absent.conf"), "")
		if err := cmd.ParseFlags(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := f.settle(cmd); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if f.cfg.TrustDomain != tc.td || f.adminSocket != tc.admin || f.relay.ID != tc.relayID || f.cfg.Relay != &f.relay {
			t.Errorf("%v: trust domain %q, admin socket %q, relay %q (%v); want %q, %q, %q", tc.args, f.cfg.TrustDomain, f.adminSocket, f.relay.ID, f.cfg.Relay, tc.td, tc.admin, tc.relayID)
		}
	}
}

// TestSettleRefusesAMissingFile: a settings file named on the command line
// must exist.
func TestSettleRefusesAMissingFile(t *testing.T) {
	var f flags
	cmd := command(&f)
	cmd.Flags().String(config.FlagName, config.DefaultPath, "")
	if err := cmd.ParseFlags([]string{"--config", filepath.Join(t.TempDir(), "absent.conf")}); err != nil {
		t.Fatal(err)
	}
	if err := f.settle(cmd); err == nil {
		t.Error("settle with a missing --config: no error")
	}
}
