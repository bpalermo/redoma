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
