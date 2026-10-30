package ocihook

import (
	"slices"
	"testing"
	"time"

	"github.com/bpalermo/redoma/common/config"
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
	h := newHook(flags{socket: "/run/redoma-xds/register.sock", netnsDir: "/run/netns", annotation: "redoma/agent", timeout: time.Second}, conf)
	for _, want := range append(slices.Clone(hook.ForbiddenSources), "/srv/spire/workload", "/srv/spire/broker", "/srv/spire/admin") {
		if !slices.Contains(h.Forbidden, want) {
			t.Errorf("the hook does not forbid %s: %q", want, h.Forbidden)
		}
	}
}
