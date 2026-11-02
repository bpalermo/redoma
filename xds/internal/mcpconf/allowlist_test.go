package mcpconf

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestReadAllowlist(t *testing.T) {
	d := testDir(t)
	write(t, d.AllowlistPath("grafana", "a"), `# reads on two datasources, search anywhere
tool = query_prometheus
tool = list_prometheus_label_values
constrain = * arguments.datasourceUid = prom-main
constrain = query_prometheus arguments.queryType ^= range
[set]
tool = query_loki_logs
constrain = * arguments.datasourceUid = loki-main
[set]
tool_unconstrained = search_dashboards
`)
	got, err := d.ReadAllowlist("grafana", "a")
	if err != nil {
		t.Fatal(err)
	}
	want := &Allowlist{Sets: []Set{
		{Tools: []Tool{
			{Name: "query_prometheus", Constraints: []Constraint{
				{Path: []string{"arguments", "datasourceUid"}, Value: "prom-main"},
				{Path: []string{"arguments", "queryType"}, Value: "range", Prefix: true},
			}},
			{Name: "list_prometheus_label_values", Constraints: []Constraint{
				{Path: []string{"arguments", "datasourceUid"}, Value: "prom-main"},
			}},
		}},
		{Tools: []Tool{
			{Name: "query_loki_logs", Constraints: []Constraint{
				{Path: []string{"arguments", "datasourceUid"}, Value: "loki-main"},
			}},
		}},
		{Tools: []Tool{{Name: "search_dashboards"}}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadAllowlist =\n%+v\nwant\n%+v", got, want)
	}
	if tools := got.Tools(); !slices.Equal(tools, []string{"list_prometheus_label_values", "query_loki_logs", "query_prometheus", "search_dashboards"}) {
		t.Errorf("Tools = %q", tools)
	}
	if paths := got.Arguments(); !slices.Equal(paths, []string{"arguments.datasourceUid", "arguments.queryType"}) {
		t.Errorf("Arguments = %q", paths)
	}
}

// The defaults' sets come first, then the agent's own.
func TestReadAllowlistDefaultsFirst(t *testing.T) {
	d := testDir(t)
	write(t, d.AllowlistPath("u", Default), "tool_unconstrained = ping_tool\n")
	write(t, d.AllowlistPath("u", "a"), "tool_unconstrained = own_tool\n")
	got, err := d.ReadAllowlist("u", "a")
	if err != nil {
		t.Fatal(err)
	}
	want := &Allowlist{Sets: []Set{{Tools: []Tool{{Name: "ping_tool"}}}, {Tools: []Tool{{Name: "own_tool"}}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadAllowlist = %+v, want %+v", got, want)
	}
}

// Without the agent's own file the agent has no route to the upstream: the
// defaults alone give no agent an upstream.
func TestReadAllowlistWithoutTheAgentsFile(t *testing.T) {
	d := testDir(t)
	write(t, d.AllowlistPath("u", Default), "tool_unconstrained = t\n")
	if got, err := d.ReadAllowlist("u", "a"); got != nil || err != nil {
		t.Errorf("ReadAllowlist = %+v, %v; want nil, nil", got, err)
	}
}

// An empty file is an upstream with no tool allowed: a route, every call
// refused.
func TestReadAllowlistEmptyFile(t *testing.T) {
	d := testDir(t)
	write(t, d.AllowlistPath("u", "a"), "# nothing yet\n")
	got, err := d.ReadAllowlist("u", "a")
	if err != nil || got == nil || len(got.Tools()) != 0 {
		t.Errorf("ReadAllowlist = %+v, %v; want an empty list", got, err)
	}
}

func TestReadAllowlistRefuses(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"unknown key", "tools = t\n", `unknown key "tools"`},
		{"bad tool name", "tool = a b\n", "tool name"},
		{"empty tool", "tool =\n", "tool name"},
		{"listed twice", "tool = t\ntool = t\n", "listed twice"},
		{"both ways", "tool = t\ntool_unconstrained = t\n", "listed twice"},
		{"constraint fields", "tool = t\nconstrain = t arguments.a\n", "constrain"},
		{"constraint op", "tool = t\nconstrain = t arguments.a ~= b\n", "constrain"},
		{"constraint outside arguments", "tool = t\nconstrain = t params.name = b\n", "arguments."},
		{"constraint empty segment", "tool = t\nconstrain = t arguments..a = b\n", "arguments."},
		{"constraint bare arguments", "tool = t\nconstrain = t arguments = b\n", "arguments."},
		{"constraint unknown tool", "tool = t\nconstrain = u arguments.a = b\n", `"u" is no tool = line of its set`},
		{"constraint on unconstrained", "tool_unconstrained = t\nconstrain = t arguments.a = b\n", `"t" is no tool = line of its set`},
		{"constraint before its tool", "constrain = t arguments.a = b\ntool = t\n", `"t" is no tool = line of its set`},
		{"constraint in another set", "tool = t\nconstrain = t arguments.a = b\n[set]\nconstrain = t arguments.a = b\n", `"t" is no tool = line of its set`},
		// A tool = line no constrain line reaches would allow the tool with
		// any arguments: refused, never read as unconstrained.
		{"a lone tool", "tool = t\n", "tool t: no constrain line of its set reaches it"},
		{"a tool after the star", "tool = a\nconstrain = * arguments.repo = r\ntool = b\n", "tool b: no constrain line"},
		{"a tool after the star, with a later set", "tool = pull_request_read\nconstrain = * arguments.repo = example-repo\ntool = create_pull_request\n[set]\ntool_unconstrained = x\n", "tool create_pull_request: no constrain line"},
		{"a lone tool in a later set", "tool_unconstrained = x\n[set]\ntool = t\n", "tool t: no constrain line"},
		{"a tool another tool's constraint skips", "tool = a\ntool = b\nconstrain = a arguments.x = y\n", "tool b: no constrain line"},
		{"a trailing comment", "tool_unconstrained = t # note\n", "starts a comment only at the start of a line"},
		{"star with no tools", "constrain = * arguments.a = b\n", "no tool = line"},
		{"bad section", "[sets]\n", "[set]"},
		{"empty set", "[set]\n[set]\ntool = t\n", "empty set"},
		{"carriage return", "tool = t\r\n", "carriage return"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDir(t)
			write(t, d.AllowlistPath("u", "a"), tc.content)
			got, err := d.ReadAllowlist("u", "a")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ReadAllowlist = %+v, %v; want an error with %q", got, err, tc.want)
			}
		})
	}
}

// A bad defaults file fails every agent's list for the upstream.
// The refusal of an unreached tool = line points at that line.
func TestReadAllowlistUnconstrainedToolLine(t *testing.T) {
	d := testDir(t)
	write(t, d.AllowlistPath("u", "a"), "tool = a\nconstrain = * arguments.repo = r\ntool = b\n[set]\ntool_unconstrained = c\n")
	_, err := d.ReadAllowlist("u", "a")
	if err == nil || !strings.Contains(err.Error(), "a.conf:3:") || !strings.Contains(err.Error(), "tool_unconstrained = b") {
		t.Errorf("err = %v; want line 3 and the tool_unconstrained hint", err)
	}
}

func TestReadAllowlistRefusesBadDefaults(t *testing.T) {
	d := testDir(t)
	write(t, d.AllowlistPath("u", Default), "tool = a b\n")
	write(t, d.AllowlistPath("u", "a"), "tool_unconstrained = t\n")
	if _, err := d.ReadAllowlist("u", "a"); err == nil || !strings.Contains(err.Error(), "_default.conf") {
		t.Errorf("err = %v; want the defaults' error", err)
	}
}

func TestReadAllowlistRefusesBadNames(t *testing.T) {
	d := testDir(t)
	for _, tc := range [][2]string{{"u", Default}, {"u", "A"}, {"_x", "a"}, {"u", "../a"}} {
		if _, err := d.ReadAllowlist(tc[0], tc[1]); err == nil {
			t.Errorf("ReadAllowlist(%q, %q): err = nil", tc[0], tc[1])
		}
	}
}

// The directories above an allowlist are checked up to the configuration
// directory, not only the file's own.
func TestReadAllowlistRefusesAnUntrustedParent(t *testing.T) {
	d := testDir(t)
	write(t, d.AllowlistPath("u", "a"), "tool_unconstrained = t\n")
	if err := os.Chmod(filepath.Join(d.Etc, "mcp"), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadAllowlist("u", "a"); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Errorf("a world-writable mcp/: err = %v", err)
	}
}
