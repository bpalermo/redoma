package lds

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/bpalermo/redoma/xds/internal/egress"
	"github.com/bpalermo/redoma/xds/internal/modelauth"

	"github.com/google/go-cmp/cmp"
)

// testdata/two-agents.json is the listeners of two running agents, alpha
// with no egress allowlist and beta with testdata/etc/egress/beta.txt, in
// the api-key mode, written as the Envoy JSON of each listener, field for
// field: a change to what Listeners builds is a change to this file, read
// in review.
//
// testdata/model-bearer.json and testdata/model-none.json are alpha's
// model listener from it with only the credential changed, for the other two
// modes; with H='.filter_chains[0].filters[0].typed_config' and
// R="$H.route_config.virtual_hosts[0].routes[0,1].request_headers_to_remove":
//
//	jq --indent 1 "{resources: [.resources[0] | $R = [\"x-api-key\"] |
//	  $H.http_filters[1].typed_config.credential.typed_config.header = \"authorization\"]}" \
//	  testdata/two-agents.json > testdata/model-bearer.json
//	jq --indent 1 "{resources: [.resources[0] | $R = [\"authorization\", \"x-api-key\"] |
//	  del($H.http_filters[1])]}" testdata/two-agents.json > testdata/model-none.json
func golden(t *testing.T, path string) []*listenerv3.Listener {
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
	var out []*listenerv3.Listener
	for i, r := range file.Resources {
		// Each resource is the JSON of an Any: its @type and the listener's
		// fields. Unmarshalling it resolves every typed_config by name, so a
		// type the golden uses and this package does not link fails here.
		var a anypb.Any
		if err := protojson.Unmarshal(r, &a); err != nil {
			t.Fatalf("%s: resource %d: %v", path, i, err)
		}
		l := &listenerv3.Listener{}
		if err := a.UnmarshalTo(l); err != nil {
			t.Fatalf("%s: resource %d: %v", path, i, err)
		}
		out = append(out, l)
	}
	return out
}

// TestListenersMatchGolden: the listeners served over LDS are the golden's,
// field for field, typed configs included.
func TestListenersMatchGolden(t *testing.T) {
	want := golden(t, "testdata/two-agents.json")

	// The golden lists the agents in sorted order.
	var got []*listenerv3.Listener
	for _, repo := range []string{"alpha", "beta"} {
		hosts, err := egress.Hosts("testdata/etc", repo)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, Listeners("/run/netns/redoma-"+repo, repo, modelauth.APIKey, hosts)...)
	}

	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("listeners differ from the golden (-golden +lds):\n%s", diff)
	}
}

// TestModelListenerPerMode: bearer replaces authorization's value and strips
// x-api-key; none injects nothing and strips both. Nothing else differs from
// the api-key listener.
func TestModelListenerPerMode(t *testing.T) {
	for _, tc := range []struct {
		auth   modelauth.Mode
		golden string
	}{
		{modelauth.Bearer, "testdata/model-bearer.json"},
		{modelauth.None, "testdata/model-none.json"},
	} {
		want := golden(t, tc.golden)
		got := Listeners("/run/netns/redoma-alpha", "alpha", tc.auth, nil)[:1]
		if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
			t.Errorf("%v: model listener differs from %s (-golden +lds):\n%s", tc.auth, tc.golden, diff)
		}
	}
}

// TestModelListenerForwardsOnlyInference: in every mode, only the exact
// paths of the Messages API and its token count reach api.anthropic.com;
// the last route answers anything else with a local 403, which sends
// nothing upstream and so no credential.
func TestModelListenerForwardsOnlyInference(t *testing.T) {
	for _, auth := range []modelauth.Mode{modelauth.APIKey, modelauth.Bearer, modelauth.None} {
		l := Listeners("/run/netns/redoma-a", "a", auth, nil)[0]
		hcm := &hcmv3.HttpConnectionManager{}
		if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
			t.Fatal(err)
		}
		vhosts := hcm.GetRouteConfig().GetVirtualHosts()
		if len(vhosts) != 1 {
			t.Fatalf("%v: %d virtual hosts, want 1", auth, len(vhosts))
		}
		var forwarded []string
		routes := vhosts[0].GetRoutes()
		for _, r := range routes[:len(routes)-1] {
			if r.GetRoute().GetCluster() != anthropicCluster {
				t.Errorf("%v: route %v does not forward to %s", auth, r.GetMatch(), anthropicCluster)
			}
			forwarded = append(forwarded, r.GetMatch().GetPath())
		}
		if want := []string{"/v1/messages", "/v1/messages/count_tokens"}; !slices.Equal(forwarded, want) {
			t.Errorf("%v: forwarded paths %q, want exactly %q", auth, forwarded, want)
		}
		last := routes[len(routes)-1]
		if last.GetMatch().GetPrefix() != "/" || last.GetDirectResponse().GetStatus() != 403 {
			t.Errorf("%v: the last route is %v, want / answered with a 403", auth, last)
		}
	}
}

// TestEgressListenerIgnoresMode: the mode is the model listener's alone.
func TestEgressListenerIgnoresMode(t *testing.T) {
	want := Listeners("/run/netns/redoma-a", "a", modelauth.APIKey, []string{"github.com"})[1]
	for _, auth := range []modelauth.Mode{modelauth.Bearer, modelauth.None} {
		if got := Listeners("/run/netns/redoma-a", "a", auth, []string{"github.com"})[1]; !proto.Equal(got, want) {
			t.Errorf("%v: the egress listener changed with the mode", auth)
		}
	}
}

func TestListenersAreBoundInTheAgentsNamespace(t *testing.T) {
	for _, l := range Listeners("/run/netns/redoma-a", "a", modelauth.APIKey, nil) {
		got := l.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath()
		if got != "/run/netns/redoma-a" {
			t.Errorf("%s: network_namespace_filepath = %q", l.GetName(), got)
		}
	}
}

// TestListenersAreDeterministic: the same agent's listeners are the same
// bytes every time, so a snapshot that sends them again does not change them.
func TestListenersAreDeterministic(t *testing.T) {
	for _, auth := range []modelauth.Mode{modelauth.APIKey, modelauth.Bearer, modelauth.None} {
		first := Listeners("/run/netns/redoma-a", "a", auth, []string{"github.com", "example.com"})
		for range 20 {
			again := Listeners("/run/netns/redoma-a", "a", auth, []string{"github.com", "example.com"})
			for i := range first {
				if !proto.Equal(first[i], again[i]) {
					t.Fatalf("%v: %s differs from one build to the next", auth, first[i].GetName())
				}
			}
		}
	}
}

func TestListenersValidate(t *testing.T) {
	for _, auth := range []modelauth.Mode{modelauth.APIKey, modelauth.Bearer, modelauth.None} {
		for _, l := range Listeners("/run/netns/redoma-a", "a", auth, []string{"github.com"}) {
			if err := l.ValidateAll(); err != nil {
				t.Errorf("%v: %s: %v", auth, l.GetName(), err)
			}
		}
	}
}
