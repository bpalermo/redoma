package xds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/xds/internal/hostcheck"
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

// settled parses args, with the settings file conf, and settles them.
func settled(t *testing.T, conf string, args ...string) (flags, error) {
	t.Helper()
	var f flags
	cmd := command(&f)
	cmd.Flags().String(config.FlagName, conf, "")
	if err := cmd.ParseFlags(append([]string{"--config", conf}, args...)); err != nil {
		t.Fatal(err)
	}
	return f, f.settle(cmd)
}

// TestSettleIdentityAPI: Delegated unless the file or a flag says broker;
// Broker mode subscribes an agent's SVID after its bind; a flag is held to
// the file's validation.
func TestSettleIdentityAPI(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "redoma.conf")
	if err := os.WriteFile(conf, []byte("identity_api = broker\nspire_broker_socket = /run/spire-broker/b.sock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.conf")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := settled(t, empty)
	if err != nil || f.identityAPI != config.IdentityDelegated || f.cfg.SVIDAfterBound {
		t.Errorf("default: %q, after bound %v, %v", f.identityAPI, f.cfg.SVIDAfterBound, err)
	}
	f, err = settled(t, conf)
	if err != nil || f.identityAPI != config.IdentityBroker || f.brokerSocket != "/run/spire-broker/b.sock" || !f.cfg.SVIDAfterBound {
		t.Errorf("the file's broker: %+v, %v", f, err)
	}
	if f.file.SpireBrokerSocket != "/run/spire-broker/b.sock" {
		t.Errorf("the file as read: %+v", f.file)
	}
	f, err = settled(t, conf, "--identity-api", "delegated")
	if err != nil || f.identityAPI != config.IdentityDelegated || f.cfg.SVIDAfterBound {
		t.Errorf("--identity-api delegated: %q, %v", f.identityAPI, err)
	}
	for _, args := range [][]string{
		{"--identity-api", "brokered"},
		{"--spire-broker-socket", "/run/redoma-spire/workload/b.sock"},
		{"--spire-workload-api", "/run/spire/agent.sock"},
	} {
		if _, err := settled(t, empty, args...); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

// TestBrokerChecks: Broker mode needs SPIRE's sockets where the OCI hook,
// reading the settings file, keeps every agent from them, and the Workload
// API's directory closed to all but the services.
func TestBrokerChecks(t *testing.T) {
	root := t.TempDir()
	spire := filepath.Join(root, "spire")
	for _, d := range []string{"workload", "broker", "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(spire, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o750); err != nil {
		t.Fatal(err)
	}
	etc := t.TempDir()
	acc := hostcheck.Accounts{Passwd: filepath.Join(etc, "passwd"), Group: filepath.Join(etc, "group")}
	for _, p := range []string{acc.Passwd, acc.Group} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file := config.Default()
	file.WorkloadAPI = "unix://" + filepath.Join(spire, "workload", "agent.sock")
	file.SpireBrokerSocket = filepath.Join(spire, "broker", "broker.sock")
	f := flags{file: file, workloadAPI: file.WorkloadAPI, brokerSocket: file.SpireBrokerSocket}
	if err := brokerChecks(f, acc); err != nil {
		t.Errorf("the file's sockets: %v", err)
	}
	// A flag that moves a socket where the hook, reading the file, does not
	// forbid it.
	moved := f
	moved.brokerSocket = filepath.Join(elsewhere, "broker.sock")
	if err := brokerChecks(moved, acc); err == nil || !strings.Contains(err.Error(), "does not keep") {
		t.Errorf("a broker socket the hook does not forbid: %v", err)
	}
	if err := os.Chmod(filepath.Join(spire, "workload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := brokerChecks(f, acc); err == nil || !strings.Contains(err.Error(), "grants others") {
		t.Errorf("a Workload API directory open to others: %v", err)
	}
}
