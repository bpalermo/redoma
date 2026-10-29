package cds

import (
	"encoding/json"
	"os"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/anypb"

	// A typed config of the golden, resolved by name when it is read.
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
)

var relay = Relay{Socket: "/run/redoma-relay/relay.sock", ID: "spiffe://redoma.internal/host/relay"}

func golden(t *testing.T, path string) *clusterv3.Cluster {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Resources []json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Resources) != 1 {
		t.Fatalf("%s: %d resources, want 1", path, len(file.Resources))
	}
	var a anypb.Any
	if err := protojson.Unmarshal(file.Resources[0], &a); err != nil {
		t.Fatal(err)
	}
	c := &clusterv3.Cluster{}
	if err := a.UnmarshalTo(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRelayCluster: testdata/relay.json, written by hand from the design:
// redoma-relay's unix socket, HTTP/2 with pings, TLS 1.3 with the agent's
// SVID as the client certificate, and the relay's certificate verified
// against the bundle and required to carry the relay's ID; both secrets
// over ADS.
func TestRelayCluster(t *testing.T) {
	want := golden(t, "testdata/relay.json")
	got := RelayCluster("beta", relay, "agent/beta", "spiffe://redoma.internal")
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("relay cluster differs from testdata/relay.json (-golden +cds):\n%s", diff)
	}
	if err := got.ValidateAll(); err != nil {
		t.Error(err)
	}
	for range 20 {
		if !proto.Equal(got, RelayCluster("beta", relay, "agent/beta", "spiffe://redoma.internal")) {
			t.Fatal("the relay cluster differs from one build to the next")
		}
	}
}

// TestRelayClusterPinsTheRelayID: the relay's certificate must carry the
// relay's ID, not only chain to the bundle: the bundle is a KMS root shared
// with other trust domains (other.example.org, other.example.net), so the bundle
// alone would let any of their workloads, or any workload of this one, pose
// as the relay. Fails if the SAN matcher is dropped or widened.
func TestRelayClusterPinsTheRelayID(t *testing.T) {
	c := RelayCluster("beta", relay, "agent/beta", "spiffe://redoma.internal")
	tls := &tlsv3.UpstreamTlsContext{}
	if err := c.GetTransportSocket().GetTypedConfig().UnmarshalTo(tls); err != nil {
		t.Fatal(err)
	}
	combined := tls.GetCommonTlsContext().GetCombinedValidationContext()
	if combined == nil {
		t.Fatal("no combined validation context: the bundle and the SAN match must both apply")
	}
	if got := combined.GetValidationContextSdsSecretConfig().GetName(); got != "spiffe://redoma.internal" {
		t.Errorf("validated against %q, want the trust domain's bundle", got)
	}
	sans := combined.GetDefaultValidationContext().GetMatchTypedSubjectAltNames()
	if len(sans) != 1 {
		t.Fatalf("%d SAN matchers, want exactly the relay's ID", len(sans))
	}
	if sans[0].GetSanType() != tlsv3.SubjectAltNameMatcher_URI || sans[0].GetMatcher().GetExact() != relay.ID ||
		sans[0].GetMatcher().GetIgnoreCase() {
		t.Errorf("SAN matcher %v, want URI exact %q", sans[0], relay.ID)
	}
	// The whole default context, so that nothing beside the matcher (a
	// trust_chain_verification of ACCEPT_UNTRUSTED, allow_expired_certificate,
	// another matcher kind) can loosen it.
	want := &tlsv3.CertificateValidationContext{
		MatchTypedSubjectAltNames: []*tlsv3.SubjectAltNameMatcher{{
			SanType: tlsv3.SubjectAltNameMatcher_URI,
			Matcher: &matcherv3.StringMatcher{MatchPattern: &matcherv3.StringMatcher_Exact{Exact: relay.ID}},
		}},
	}
	if got := combined.GetDefaultValidationContext(); !proto.Equal(got, want) {
		t.Errorf("the default validation context is %v, want exactly %v", got, want)
	}
	for name, loose := range map[string]func(*tlsv3.CertificateValidationContext){
		"ACCEPT_UNTRUSTED": func(c *tlsv3.CertificateValidationContext) {
			c.TrustChainVerification = tlsv3.CertificateValidationContext_ACCEPT_UNTRUSTED
		},
		"allow_expired_certificate": func(c *tlsv3.CertificateValidationContext) { c.AllowExpiredCertificate = true },
	} {
		c := proto.Clone(want).(*tlsv3.CertificateValidationContext)
		loose(c)
		if proto.Equal(c, want) {
			t.Errorf("%s would pass the check", name)
		}
	}
	if got := tls.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs(); len(got) != 1 || got[0].GetName() != "agent/beta" {
		t.Errorf("client certificate %v, want the agent's SVID", got)
	}
}
