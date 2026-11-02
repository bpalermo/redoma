package lds

import (
	"testing"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/bpalermo/redoma/xds/internal/modelauth"
)

// TestRelayListener: testdata/relay.json, written by hand from the design:
// HTTP/2 without TLS on 127.0.0.1:7070 in the agent's namespace, the one
// Connect method to the agent's relay cluster with no route timeout and no
// maximum stream duration, pings on the connection, a stream idle timeout
// above the shim's heartbeat, a 403 for anything else, and the cluster not
// validated (it comes over CDS once the agent's SVID is there).
func TestRelayListener(t *testing.T) {
	want := golden(t, "testdata/relay.json")
	got := []*listenerv3.Listener{RelayListener("/run/netns/redoma-beta", "beta")}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("relay listener differs from testdata/relay.json (-golden +lds):\n%s", diff)
	}
	if err := got[0].ValidateAll(); err != nil {
		t.Error(err)
	}
	for range 20 {
		if !proto.Equal(got[0], RelayListener("/run/netns/redoma-beta", "beta")) {
			t.Fatal("the relay listener differs from one build to the next")
		}
	}
}

// TestRelayLeavesTheOthers: the model and egress listeners are what they
// were; the relay listener is a third, apart.
func TestRelayLeavesTheOthers(t *testing.T) {
	ls := Listeners("/run/netns/redoma-a", "a", modelauth.APIKey, nil)
	if len(ls) != 2 {
		t.Fatalf("Listeners returns %d listeners, want the model and egress ones", len(ls))
	}
	for _, l := range ls {
		if l.GetName() == ListenerName("a", "relay") || l.GetAddress().GetSocketAddress().GetPortValue() == RelayPort {
			t.Errorf("%s: the relay's name or port", l.GetName())
		}
	}
}

// TestAWSListener: testdata/aws.json, written by hand from the design:
// HTTP/1.1 on 127.0.0.1:<port> in the agent's namespace, GET
// /v1/credentials to the agent's own AWS cluster with a 5s timeout, a 404
// for any other path, and the cluster not validated.
func TestAWSListener(t *testing.T) {
	want := golden(t, "testdata/aws.json")
	got := []*listenerv3.Listener{AWSListener("/run/netns/redoma-beta", "beta", DefaultAWSPort)}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("AWS listener differs from testdata/aws.json (-golden +lds):\n%s", diff)
	}
	if err := got[0].ValidateAll(); err != nil {
		t.Error(err)
	}
	for range 20 {
		if !proto.Equal(got[0], AWSListener("/run/netns/redoma-beta", "beta", DefaultAWSPort)) {
			t.Fatal("the AWS listener differs from one build to the next")
		}
	}
	if p := AWSListener("/run/netns/redoma-beta", "beta", 8999).GetAddress().GetSocketAddress().GetPortValue(); p != 8999 {
		t.Errorf("port %d, want the one asked for", p)
	}
}

// TestTunnelListener: testdata/tunnel.json, written by hand from the
// design: the tunnel's unix socket, 0666 (the directory keeps others out),
// every connection carried as bytes to the endpoint's cluster.
func TestTunnelListener(t *testing.T) {
	const host = "rolesanywhere.us-east-1.amazonaws.com"
	want := golden(t, "testdata/tunnel.json")
	got := []*listenerv3.Listener{TunnelListener(host, "/run/redoma/xds/tunnel/"+host+".sock")}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("tunnel listener differs from testdata/tunnel.json (-golden +lds):\n%s", diff)
	}
	if err := got[0].ValidateAll(); err != nil {
		t.Error(err)
	}
}
