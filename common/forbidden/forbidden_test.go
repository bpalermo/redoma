package forbidden

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bpalermo/redoma/common/config"
)

// TestSourcesFollowTheSettings: SPIRE's sockets moved elsewhere are
// forbidden where the settings put them, and where that resolves.
func TestSourcesFollowTheSettings(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "spire-real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "spire-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.WorkloadAPI = "unix://" + link + "/agent.sock"
	c.SpireBrokerSocket = "/srv/spire-broker/broker.sock"
	got := Sources(c)
	for _, want := range append(slices.Clone(Base), link, real, "/srv/spire-broker", "/run/redoma-spire/admin") {
		if !slices.Contains(got, want) {
			t.Errorf("Sources lacks %s: %q", want, got)
		}
	}
	if !Under(filepath.Join(real, "agent.sock"), got) {
		t.Error("the resolved Workload API socket is not under Sources")
	}
}

// TestBaseCoversTheRuntimeDirectory: /run/redoma, each of its directories
// and anything in them are forbidden; a sibling that only starts with the
// same name is not forbidden by that entry, and the per-service directories
// of before are no longer listed.
func TestBaseCoversTheRuntimeDirectory(t *testing.T) {
	for _, p := range []string{
		"/run/redoma",
		"/run/redoma/",
		"/run/redoma/xds",
		"/run/redoma/xds/xds.sock",
		"/run/redoma/xds/register.sock",
		"/run/redoma/relay",
		"/run/redoma/relay/relay.sock",
		"/run/redoma/lead",
		"/run/redoma/lead/lead.sock",
		"/run/redoma/names",
		"/run/redoma/names/a",
		"/run/redoma/envoy",
		"/run/redoma/envoy/admin.sock",
		"/run/redoma/active/a",
		"/run/./redoma/lead/../xds",
	} {
		if !Under(p, Base) {
			t.Errorf("%s is not under Base", p)
		}
	}
	for _, p := range []string{"/run/redomax", "/run/redomax/xds", "/run/redoma-xds", "/run/redoma-relay"} {
		if Under(p, []string{"/run/redoma"}) {
			t.Errorf("%s is under /run/redoma", p)
		}
	}
	for _, p := range []string{"/run/redoma-xds", "/run/redoma-relay", "/run/redoma-relay-lead", "/run/redoma-names"} {
		if Under(p, Base) {
			t.Errorf("%s, a path of before /run/redoma held them all, is still under Base", p)
		}
	}
}

func TestUnder(t *testing.T) {
	sources := []string{"/run/redoma-spire", "/srv/x/"}
	for path, want := range map[string]bool{
		"/run/redoma-spire":                     true,
		"/run/redoma-spire/workload/agent.sock": true,
		"/run/redoma-spire/../redoma-spire/b":   true,
		"/run/redoma-spirex/agent.sock":         false,
		"/run":                                  false,
		"/srv/x/y":                              true,
	} {
		if got := Under(path, sources); got != want {
			t.Errorf("Under(%s) = %v, want %v", path, got, want)
		}
	}
}
