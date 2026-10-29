package controlplane

import (
	"maps"
	"slices"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	pb "google.golang.org/protobuf/proto"

	"github.com/bpalermo/redoma/xds/internal/activedir"
	"github.com/bpalermo/redoma/xds/internal/cds"
	"github.com/bpalermo/redoma/xds/internal/lds"
)

var testRelay = cds.Relay{Socket: "/run/redoma-relay/relay.sock", ID: "spiffe://" + td + "/host/relay"}

func clusters(v view) []string {
	if v.snap == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(v.snap.GetResources(resource.ClusterType)))
}

// TestRelay: with a relay, each agent has a third listener and, once its
// SVID is there, a cluster to the relay that presents it; both go with the
// agent, and the cluster with the SVID.
func TestRelay(t *testing.T) {
	e := newEnvWith(t, func(c *Config) { c.Relay = &testRelay })
	e.startAgent(t, "a", "")
	v := e.eventually(t, "a's relay listener and cluster", func(v view) bool {
		return slices.Equal(v.listeners, []string{"agent_a_egress", "agent_a_model", "agent_a_relay"}) &&
			slices.Equal(clusters(v), []string{"agent_a_relay"})
	})
	got := v.snap.GetResources(resource.ListenerType)["agent_a_relay"].(*listenerv3.Listener)
	if !pb.Equal(got, lds.RelayListener(activedir.Namespace(e.src.NetnsDir, "a"), "a")) {
		t.Error("a's relay listener is not lds.RelayListener's")
	}
	c := v.snap.GetResources(resource.ClusterType)["agent_a_relay"].(*clusterv3.Cluster)
	if !pb.Equal(c, cds.RelayCluster("a", testRelay, "agent/a", "spiffe://"+td)) {
		t.Error("a's relay cluster is not cds.RelayCluster's for its secret and the bundle")
	}

	// SPIRE drops the entry: the cluster goes with the secret, so Envoy
	// never warms one waiting for it; the listeners stay.
	e.fake.SetSVIDs("redoma:agent:a")
	v = e.eventually(t, "a's cluster gone", func(v view) bool { return len(clusters(v)) == 0 })
	if !slices.Contains(v.listeners, "agent_a_relay") {
		t.Error("the relay listener went with the SVID")
	}
	e.fake.SetSVIDs("redoma:agent:a", e.ca.SVID(t, "/agent/a"))
	e.eventually(t, "a's cluster back", func(v view) bool { return slices.Equal(clusters(v), []string{"agent_a_relay"}) })

	e.stopAgent(t, "a")
	e.eventually(t, "a gone", func(v view) bool { return len(v.listeners) == 0 && len(clusters(v)) == 0 })
}

// TestNoRelay: without one, the agents have their two listeners and no
// cluster, as before.
func TestNoRelay(t *testing.T) {
	e := newEnv(t)
	e.startAgent(t, "a", "")
	v := e.eventually(t, "a", func(v view) bool { return slices.Contains(v.secrets, "agent/a") })
	if !slices.Equal(v.listeners, []string{"agent_a_egress", "agent_a_model"}) || len(clusters(v)) != 0 {
		t.Errorf("listeners %q, clusters %q; want the two and none", v.listeners, clusters(v))
	}
}
