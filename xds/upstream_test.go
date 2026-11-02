package xds

import (
	"os"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/xds/internal/mcpconf"
	"github.com/bpalermo/redoma/xds/internal/upstreamrun"
)

func unit(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestUpstreamUnits: the units run this command's subcommands, stop the
// container run names, and make the directory the credential's token_file
// must be in.
func TestUpstreamUnits(t *testing.T) {
	run := unit(t, "redoma-mcp@.service")
	for _, want := range []string{
		"\nExecStart=/usr/bin/redoma mcp-upstream run %i\n",
		"\nExecStop=/usr/bin/podman stop --ignore --time 10 " + upstreamrun.ContainerName("%i") + "\n",
		"\nAfter=redoma-xds.service redoma-mcp-token@%i.service\n",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("redoma-mcp@.service lacks %q", want)
		}
	}
	token := unit(t, "redoma-mcp-token@.service")
	for _, want := range []string{
		"\nExecStart=/usr/bin/redoma mcp-upstream token %i ${REDOMA_MCP_TOKEN_SOURCE}\n",
		"\nRuntimeDirectory=" + strings.TrimSuffix(strings.TrimPrefix(mcpconf.TokenDir("%i"), "/run/"), "/") + "\n",
		"\nRuntimeDirectoryMode=0700\n",
		"\nRuntimeDirectoryPreserve=yes\n",
	} {
		if !strings.Contains(token, want) {
			t.Errorf("redoma-mcp-token@.service lacks %q", want)
		}
	}
	path := unit(t, "redoma-mcp-token@.path")
	if !strings.Contains(path, "\nUnit=redoma-mcp-token@%i.service\n") || strings.Contains(path, "\nPathChanged=") {
		t.Error("redoma-mcp-token@.path must start the service and name no path of its own")
	}

	sub := map[string]bool{}
	for _, c := range UpstreamCommand().Commands() {
		sub[c.Name()] = true
	}
	if !sub["run"] || !sub["token"] {
		t.Errorf("mcp-upstream subcommands %v", sub)
	}
}
