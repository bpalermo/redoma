package ocihook

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/common/forbidden"
	"github.com/bpalermo/redoma/ocihook/internal/hook"
)

// TestHookForbidsTheConfiguredSockets: SPIRE's sockets moved out of
// /run/redoma-spire by the settings are forbidden where they are, so the
// hook's mount check still keeps them from the agents.
func TestHookForbidsTheConfiguredSockets(t *testing.T) {
	conf := config.Default()
	conf.WorkloadAPI = "unix:///srv/spire/workload/agent.sock"
	conf.SpireBrokerSocket = "/srv/spire/broker/broker.sock"
	conf.SpireAdminSocket = "/srv/spire/admin/admin.sock"
	h := newHook(flags{socket: "/run/redoma/xds/register.sock", netnsDir: "/run/netns", annotation: "redoma/agent", timeout: time.Second}, conf)
	for _, want := range append(slices.Clone(hook.ForbiddenSources), "/srv/spire/workload", "/srv/spire/broker", "/srv/spire/admin") {
		if !slices.Contains(h.Forbidden, want) {
			t.Errorf("the hook does not forbid %s: %q", want, h.Forbidden)
		}
	}
}

// TestHookForbidsItsSocketsDirectory: a host that keeps redoma xds's
// sockets outside /run/redoma (here at a path of before, through a symlink)
// still has their directory forbidden, as written and as it resolves.
func TestHookForbidsItsSocketsDirectory(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "xds-real")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "redoma-xds")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	h := newHook(flags{socket: filepath.Join(link, "register.sock"), netnsDir: "/run/netns", annotation: "redoma/agent", timeout: time.Second}, config.Default())
	for _, want := range []string{link, real, "/run/redoma"} {
		if !slices.Contains(h.Forbidden, want) {
			t.Errorf("the hook does not forbid %s: %q", want, h.Forbidden)
		}
	}
	if !forbidden.Under(filepath.Join(real, "xds.sock"), h.Forbidden) {
		t.Error("a socket beside the registration socket is not forbidden")
	}
	for _, socket := range []string{"", "register.sock"} {
		got := newHook(flags{socket: socket}, config.Default()).Forbidden
		if !slices.Equal(got, forbidden.Sources(config.Default())) {
			t.Errorf("--socket %q changed the forbidden paths: %q", socket, got)
		}
	}
}
