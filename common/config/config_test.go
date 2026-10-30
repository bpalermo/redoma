package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParse: each key over the defaults, comments and blank lines ignored,
// spaces around the key and the value trimmed.
func TestParse(t *testing.T) {
	c, err := Parse(strings.NewReader(`
# the host's trust domain
  trust_domain=example.org
agent_annotation = example.org/agent

spire_admin_socket = /run/spire/admin.sock
spire_workload_api = unix:///run/spire/agent.sock
identity_api = broker
spire_broker_socket = /run/spire-broker/broker.sock
`))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		TrustDomain:       "example.org",
		AgentAnnotation:   "example.org/agent",
		SpireAdminSocket:  "/run/spire/admin.sock",
		WorkloadAPI:       "unix:///run/spire/agent.sock",
		IdentityAPI:       IdentityBroker,
		SpireBrokerSocket: "/run/spire-broker/broker.sock",
	}
	if c != want {
		t.Errorf("Parse = %+v, want %+v", c, want)
	}
	if got := c.RelayID(); got != "spiffe://example.org/host/relay" {
		t.Errorf("RelayID = %q", got)
	}
}

// TestDefault: an empty file is the defaults, and the defaults are valid.
func TestDefault(t *testing.T) {
	c, err := Parse(strings.NewReader("# nothing set\n"))
	if err != nil || c != Default() {
		t.Fatalf("Parse(empty) = %+v, %v; want %+v", c, err, Default())
	}
	if c.TrustDomain != "redoma.internal" || c.AgentAnnotation != "redoma/agent" || c.RelayID() != "spiffe://redoma.internal/host/relay" {
		t.Errorf("defaults: %+v", c)
	}
}

// TestParseRefuses: a file with any bad line is refused whole.
func TestParseRefuses(t *testing.T) {
	for in, want := range map[string]string{
		"trust_domain":                               "not `key = value`",
		"= redoma.internal":                          "not `key = value`",
		"trustdomain = example.org":                  "unknown setting",
		"Trust_Domain = example.org":                 "unknown setting",
		"trust_domain = a.org\ntrust_domain = b.org": "set twice",
		"trust_domain = Example.Org":                 "trust_domain",
		"trust_domain = spiffe://example.org":        "trust_domain",
		"trust_domain =":                             "trust_domain",
		"agent_annotation = a b":                     "agent_annotation",
		"agent_annotation = a=b":                     "agent_annotation",
		"spire_admin_socket = admin.sock":            "spire_admin_socket",
		"spire_workload_api = /run/spire/agent.sock": "spire_workload_api",
		"spire_workload_api = tcp://192.0.2.1:8081":  "spire_workload_api",
		"identity_api = Broker":                      "identity_api",
		"identity_api =":                             "identity_api",
		"spire_broker_socket = broker.sock":          "spire_broker_socket",
		// SPIRE refuses a broker socket in the Workload API's directory,
		// or in any directory below it.
		"spire_broker_socket = /run/redoma-spire/workload/broker.sock":                                   "spire_broker_socket",
		"spire_broker_socket = /run/redoma-spire/workload/b/broker.sock":                                 "spire_broker_socket",
		"spire_workload_api = unix:///run/s/agent.sock\nspire_broker_socket = /run/s/../s/x/broker.sock": "spire_broker_socket",
	} {
		if _, err := Parse(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want an error with %q", in, err, want)
		}
	}
}

// TestBrokerSocketBesideTheWorkloadAPI: a sibling directory, or one whose
// name only starts like the Workload API's, is no subdirectory of it.
func TestBrokerSocketBesideTheWorkloadAPI(t *testing.T) {
	for _, in := range []string{
		"spire_broker_socket = /run/redoma-spire/broker/broker.sock",
		"spire_broker_socket = /run/redoma-spire/workload-broker/broker.sock",
		"spire_workload_api = unix:///run/s/w/agent.sock\nspire_broker_socket = /run/s/broker.sock",
	} {
		if _, err := Parse(strings.NewReader(in)); err != nil {
			t.Errorf("Parse(%q) = %v", in, err)
		}
	}
	if s := Default().SpireSockets(); len(s) != 3 || s[1] != "/run/redoma-spire/workload/agent.sock" {
		t.Errorf("SpireSockets = %q", s)
	}
}

// TestLoad: a missing file is the defaults unless it is required; a bad
// one names its path.
func TestLoad(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.conf")
	if c, err := Load(missing, false); err != nil || c != Default() {
		t.Errorf("Load(missing, false) = %+v, %v", c, err)
	}
	if _, err := Load(missing, true); err == nil {
		t.Error("Load(missing, true): no error")
	}
	bad := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(bad, []byte("frob = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad, false); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("Load(bad) = %v, want an error naming the file", err)
	}
	good := filepath.Join(dir, "redoma.conf")
	if err := os.WriteFile(good, []byte("trust_domain = example.org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(good, true); err != nil || c.TrustDomain != "example.org" || c.AgentAnnotation != Default().AgentAnnotation {
		t.Errorf("Load(good) = %+v, %v", c, err)
	}
}
