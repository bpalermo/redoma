package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/common/version"
)

// TestSubcommands: every role is a subcommand of the one binary.
func TestSubcommands(t *testing.T) {
	var got []string
	for _, c := range root().Commands() {
		got = append(got, c.Name())
	}
	for _, want := range []string{"mcp", "msg", "oci-hook", "relay", "xds"} {
		if !slices.Contains(got, want) {
			t.Errorf("no subcommand %q in %v", want, got)
		}
	}
	msg, _, err := root().Find([]string{"msg"})
	if err != nil {
		t.Fatal(err)
	}
	var subs []string
	for _, c := range msg.Commands() {
		subs = append(subs, c.Name())
	}
	slices.Sort(subs)
	if !slices.Equal(subs, []string{"cancel", "reply", "send", "watch"}) {
		t.Errorf("redoma msg has %v", subs)
	}
}

// TestMissingConfigRefused: a settings file named with --config must exist,
// and the failure comes before a service starts.
func TestMissingConfigRefused(t *testing.T) {
	for _, sub := range [][]string{{"xds"}, {"relay"}, {"oci-hook", "poststop"}} {
		r := root()
		r.SetArgs(append([]string{"--config", filepath.Join(t.TempDir(), "absent.conf")}, sub...))
		r.SetIn(strings.NewReader("{}"))
		if err := r.Execute(); err == nil || !strings.Contains(err.Error(), "absent.conf") {
			t.Errorf("redoma %v with a missing --config: %v", sub, err)
		}
	}
}

// TestVersion: --version prints the build version.
func TestVersion(t *testing.T) {
	var out bytes.Buffer
	r := root()
	r.SetArgs([]string{"--version"})
	r.SetOut(&out)
	if err := r.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), version.Version) {
		t.Errorf("--version printed %q", out.String())
	}
}

// TestShippedConfig: the settings file the package installs parses, and
// says what the defaults say.
func TestShippedConfig(t *testing.T) {
	c, err := config.Load("redoma.conf", true)
	if err != nil {
		t.Fatal(err)
	}
	if c != config.Default() {
		t.Errorf("redoma.conf = %+v, the defaults %+v", c, config.Default())
	}
}
