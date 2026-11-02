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

// TestTokenWithoutATokenFile: every start of an upstream runs the copy;
// one without a token_file has nothing to copy, and succeeds, whatever the
// source.
func TestTokenWithoutATokenFile(t *testing.T) {
	var log strings.Builder
	if err := copyToken(&mcpconf.Upstream{Name: "u"}, "", 0, &log); err != nil || !strings.Contains(log.String(), "nothing to copy") {
		t.Errorf("no token_file: %v, %q; want success", err, log.String())
	}
	if err := copyToken(&mcpconf.Upstream{Name: "u", TokenFile: "/run/redoma-mcp/u/token"}, "", 0, &log); err == nil {
		t.Error("a token_file and no source: no error")
	}
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
		// Every start copies the credential first: after a boot /run is
		// empty, and the path unit fires only on a change.
		"\nWants=redoma-xds.service redoma-mcp-token@%i.service\n",
		// Retried for as long as the minter has not written: never failed
		// for good by the start limit.
		"\nStartLimitIntervalSec=0\n",
		"\nRestart=on-failure\n",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("redoma-mcp@.service lacks %q", want)
		}
	}
	token := unit(t, "redoma-mcp-token@.service")
	for _, want := range []string{
		"\nExecStart=/usr/bin/redoma mcp-upstream token --owner=${REDOMA_MCP_TOKEN_OWNER} %i ${REDOMA_MCP_TOKEN_SOURCE}\n",
		"\nEnvironment=REDOMA_MCP_TOKEN_OWNER=0\n",
		"\nCapabilityBoundingSet=CAP_DAC_READ_SEARCH\n",
		"\nSystemCallFilter=@system-service\n",
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
	for _, c := range UpstreamCommand().Commands() {
		if f := c.Flags().Lookup("owner"); c.Name() == "token" && (f == nil || f.DefValue != "0") {
			t.Errorf("token --owner: %v, want a flag defaulting to root", f)
		}
	}
}
