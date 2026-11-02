package controlplane

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	pb "google.golang.org/protobuf/proto"

	"github.com/bpalermo/redoma/xds/internal/activedir"
	"github.com/bpalermo/redoma/xds/internal/cds"
	"github.com/bpalermo/redoma/xds/internal/lds"
	"github.com/bpalermo/redoma/xds/internal/rolesanywhere"
	"github.com/bpalermo/redoma/xds/internal/rolesanywhere/rolesanywheretest"
)

// The test ARNs, composed from their parts by rolesanywheretest.
var (
	awsAnchor  = rolesanywheretest.Anchor("us-east-1")
	awsProfile = rolesanywheretest.Profile("us-east-1")
	awsRole    = rolesanywheretest.Role("example-agent")
)

const (
	awsPort   = 9911
	tunnelDir = "/run/redoma/xds/tunnel"
	east      = "rolesanywhere.us-east-1.amazonaws.com"
)

var tunnelEast = cds.TunnelName(east)

// fakeAWS records what the control plane tells the credentials service.
type fakeAWS struct {
	mu      sync.Mutex
	set     map[string]awsState
	removed []string
}

type awsState struct {
	profile *rolesanywhere.Profile
	err     error
	svid    bool
}

func (f *fakeAWS) Set(id string, p *rolesanywhere.Profile, err error, chainPEM, keyPEM []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.set == nil {
		f.set = map[string]awsState{}
	}
	if p == nil && err == nil {
		delete(f.set, id)
		return
	}
	f.set[id] = awsState{profile: p, err: err, svid: chainPEM != nil && keyPEM != nil}
}

func (f *fakeAWS) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.set, id)
	f.removed = append(f.removed, id)
}

func (f *fakeAWS) Socket(id string) string { return "/run/redoma/xds/aws/" + id + ".sock" }

func (f *fakeAWS) state(id string) (awsState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.set[id]
	return s, ok
}

func writeAWS(t *testing.T, e *env, repo, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(e.cfg.EtcDir, "aws"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.cfg.EtcDir, "aws", repo+".conf"), content)
}

var awsConf = "trust_anchor_arn = " + awsAnchor + "\nprofile_arn = " + awsProfile + "\nrole_arn = " + awsRole + "\n"

// TestAWS: an agent with aws/<agent>.conf has a fourth listener and a
// cluster to its own socket, and the service is told its profile and SVID;
// a broken file keeps the listener and tells the service the error; no file
// takes both away; a stopped agent is removed from the service.
func TestAWS(t *testing.T) {
	aws := &fakeAWS{}
	e := newEnvWith(t, func(c *Config) { c.AWS, c.AWSPort, c.AWSTunnelDir = aws, awsPort, tunnelDir })
	writeAWS(t, e, "a", awsConf)
	e.startAgent(t, "a", "")
	e.startAgent(t, "b", "")
	v := e.eventually(t, "a's AWS listener and cluster", func(v view) bool {
		return slices.Contains(v.listeners, "agent_a_aws") && slices.Equal(clusters(v), []string{"agent_a_aws", tunnelEast})
	})
	if slices.Contains(v.listeners, "agent_b_aws") {
		t.Error("b, with no AWS settings, has an AWS listener")
	}
	got := v.snap.GetResources(resource.ListenerType)["agent_a_aws"].(*listenerv3.Listener)
	if !pb.Equal(got, lds.AWSListener(activedir.Namespace(e.src.NetnsDir, "a"), "a", awsPort)) {
		t.Error("a's AWS listener is not lds.AWSListener's")
	}
	c := v.snap.GetResources(resource.ClusterType)["agent_a_aws"].(*clusterv3.Cluster)
	if !pb.Equal(c, cds.AWSCluster("a", aws.Socket("a"))) {
		t.Error("a's AWS cluster is not cds.AWSCluster's for its socket")
	}
	e.eventually(t, "the service has a's profile and SVID", func(view) bool {
		s, ok := aws.state("a")
		return ok && s.svid && s.err == nil && s.profile != nil && s.profile.RoleARN == awsRole
	})
	if _, ok := aws.state("b"); ok {
		t.Error("the service was told of b")
	}

	writeAWS(t, e, "a", awsConf+"unknown = 1\n")
	e.eventually(t, "the service has a's error", func(view) bool {
		s, ok := aws.state("a")
		return ok && s.err != nil && s.profile == nil
	})
	if v := e.view(); !slices.Contains(v.listeners, "agent_a_aws") {
		t.Error("a broken file took the listener away")
	}

	if err := os.Remove(filepath.Join(e.cfg.EtcDir, "aws", "a.conf")); err != nil {
		t.Fatal(err)
	}
	e.eventually(t, "a's AWS listener and cluster, and the tunnel, gone", func(v view) bool {
		return !slices.Contains(v.listeners, "agent_a_aws") && !slices.Contains(v.listeners, tunnelEast) && len(clusters(v)) == 0
	})
	if _, ok := aws.state("a"); ok {
		t.Error("the service still has a after the opt-out")
	}

	writeAWS(t, e, "a", awsConf)
	e.eventually(t, "a's AWS listener back", func(v view) bool { return slices.Contains(v.listeners, "agent_a_aws") })
	e.stopAgent(t, "a")
	e.eventually(t, "a gone", func(v view) bool { return !slices.Contains(v.listeners, "agent_a_aws") && len(clusters(v)) == 0 })
	aws.mu.Lock()
	removed := slices.Contains(aws.removed, "a")
	aws.mu.Unlock()
	if !removed {
		t.Error("the service was not told a stopped")
	}
}

// TestNoAWS: without the service, an aws/<agent>.conf changes nothing.
func TestNoAWS(t *testing.T) {
	e := newEnv(t)
	writeAWS(t, e, "a", awsConf)
	e.startAgent(t, "a", "")
	v := e.eventually(t, "a", func(v view) bool { return slices.Contains(v.secrets, "agent/a") })
	if !slices.Equal(v.listeners, []string{"agent_a_egress", "agent_a_model"}) || len(clusters(v)) != 0 {
		t.Errorf("listeners %q, clusters %q; want the two and none", v.listeners, clusters(v))
	}
}

// TestTunnels: one tunnel per Roles Anywhere endpoint that a running
// agent's valid settings name, shared by the agents of one region; none
// without such an agent, a broken file names none.
func TestTunnels(t *testing.T) {
	aws := &fakeAWS{}
	e := newEnvWith(t, func(c *Config) { c.AWS, c.AWSPort, c.AWSTunnelDir = aws, awsPort, tunnelDir })
	e.startAgent(t, "a", "")
	v := e.eventually(t, "a", func(v view) bool { return slices.Contains(v.secrets, "agent/a") })
	if !slices.Equal(v.listeners, []string{"agent_a_egress", "agent_a_model"}) || len(clusters(v)) != 0 {
		t.Fatalf("no AWS settings: listeners %q, clusters %q", v.listeners, clusters(v))
	}

	writeAWS(t, e, "a", awsConf)
	writeAWS(t, e, "b", awsConf)
	e.startAgent(t, "b", "")
	v = e.eventually(t, "one tunnel for both", func(v view) bool {
		return slices.Equal(clusters(v), []string{"agent_a_aws", "agent_b_aws", tunnelEast})
	})
	l := v.snap.GetResources(resource.ListenerType)[tunnelEast].(*listenerv3.Listener)
	if !pb.Equal(l, lds.TunnelListener(east, tunnelDir+"/"+east+".sock")) {
		t.Error("the tunnel listener is not lds.TunnelListener's for its socket")
	}
	if c := v.snap.GetResources(resource.ClusterType)[tunnelEast].(*clusterv3.Cluster); !pb.Equal(c, cds.TunnelCluster(east)) {
		t.Error("the tunnel cluster is not cds.TunnelCluster's")
	}

	west := "rolesanywhere.eu-west-1.amazonaws.com"
	writeAWS(t, e, "b", "trust_anchor_arn = "+rolesanywheretest.Anchor("eu-west-1")+"\nprofile_arn = "+rolesanywheretest.Profile("eu-west-1")+"\nrole_arn = "+awsRole+"\n")
	e.eventually(t, "a tunnel per region", func(v view) bool {
		return slices.Contains(v.listeners, tunnelEast) && slices.Contains(v.listeners, cds.TunnelName(west))
	})

	writeAWS(t, e, "a", "broken\n")
	writeAWS(t, e, "b", "broken\n")
	e.eventually(t, "no tunnel for broken settings", func(v view) bool {
		return slices.Equal(clusters(v), []string{"agent_a_aws", "agent_b_aws"}) &&
			!slices.Contains(v.listeners, tunnelEast) && !slices.Contains(v.listeners, cds.TunnelName(west))
	})
}

// TestTunnelsOncePerEndpoint: agents of one region share one tunnel, and an
// agent with no settings or broken ones names none.
func TestTunnelsOncePerEndpoint(t *testing.T) {
	east := &rolesanywhere.Profile{TrustAnchorARN: awsAnchor, ProfileARN: awsProfile, RoleARN: awsRole}
	c := &Controller{cfg: Config{AWS: &fakeAWS{}}, agents: map[string]*running{
		"a": {aws: east}, "b": {aws: east}, "c": {}, "d": {awsErr: "broken"},
	}}
	if got := c.tunnels(); !slices.Equal(got, []string{"rolesanywhere.us-east-1.amazonaws.com"}) {
		t.Errorf("tunnels %q", got)
	}
	c.cfg.AWS = nil
	if got := c.tunnels(); len(got) != 0 {
		t.Errorf("tunnels %q without the service", got)
	}
}
