package ocihook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bpalermo/redoma/common/config"
	"github.com/bpalermo/redoma/common/register"
)

// hookDef is the hooks.d schema 1.0.0 (containers/common pkg/hooks).
type hookDef struct {
	Version string `json:"version"`
	Hook    struct {
		Path    string   `json:"path"`
		Args    []string `json:"args"`
		Timeout *int     `json:"timeout"`
	} `json:"hook"`
	When struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"when"`
	Stages []string `json:"stages"`
}

func readDef(t *testing.T, name string) hookDef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("hooks.d", name))
	if err != nil {
		t.Fatal(err)
	}
	var d hookDef
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

// TestHookDefinitions: the hooks.d files run this binary at the stage its
// argument names, only for containers with the annotation, with runtime
// timeouts longer than the binary's own, so the hook fails cleanly (undoing
// its mount) before the runtime kills it.
func TestHookDefinitions(t *testing.T) {
	for file, stage := range map[string]string{
		"redoma-create-runtime.json": "createRuntime",
		"redoma-poststop.json":       "poststop",
	} {
		d := readDef(t, file)
		if d.Version != "1.0.0" || !slices.Equal(d.Stages, []string{stage}) {
			t.Errorf("%s: version %q stages %q, want 1.0.0 and [%s]", file, d.Version, d.Stages, stage)
		}
		arg := map[string]string{"createRuntime": "create-runtime", "poststop": "poststop"}[stage]
		if !slices.Equal(d.Hook.Args, []string{"redoma", "oci-hook", arg}) {
			t.Errorf("%s: args %q", file, d.Hook.Args)
		}
		if d.Hook.Path != "/usr/bin/redoma" {
			t.Errorf("%s: path %q", file, d.Hook.Path)
		}
		if len(d.When.Annotations) != 1 || d.When.Annotations["^"+regexp.QuoteMeta(config.Default().AgentAnnotation)+"$"] == "" {
			t.Errorf("%s: when %v, want the agent annotation only", file, d.When.Annotations)
		}
		if d.Hook.Timeout == nil {
			t.Errorf("%s: no timeout", file)
		}
	}

	create := readDef(t, "redoma-create-runtime.json")
	// The binary's --timeout default, plus the undo's 5s.
	if limit := time.Duration(*create.Hook.Timeout) * time.Second; limit <= 30*time.Second+5*time.Second {
		t.Errorf("createRuntime timeout %v leaves the binary (30s + 5s undo) no room", limit)
	}
	poststop := readDef(t, "redoma-poststop.json")
	if limit := time.Duration(*poststop.Hook.Timeout) * time.Second; limit <= 5*time.Second {
		t.Errorf("poststop timeout %v leaves the unregister (5s) no room", limit)
	}

	// The createRuntime match accepts agent names only (the control plane
	// checks again); poststop matches any value, so a container whose start
	// failed half-way is still cleaned up.
	match := regexp.MustCompile(create.When.Annotations[`^redoma/agent$`])
	for name, want := range map[string]bool{"beta": true, "Bad_Name": false, "../x": false} {
		if match.MatchString(name) != want {
			t.Errorf("createRuntime match of %q = %v", name, !want)
		}
	}
	// The drop-in appends the directory the definitions' path is in.
	conf, err := os.ReadFile("containers.conf.d/50-redoma-hooks.conf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "[engine]\nhooks_dir = [\"/usr/share/containers/oci/hooks.d\", {append=true}]\n") {
		t.Error("50-redoma-hooks.conf does not append /usr/share/containers/oci/hooks.d to [engine] hooks_dir")
	}
	if register.NetnsPrefix != "redoma-oci-" {
		t.Errorf("NetnsPrefix %q changed: update AGENTS.md", register.NetnsPrefix)
	}
}
