package upstreamrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/xds/internal/mcpconf"
)

const image = "docker.io/grafana/mcp-grafana@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func grafana() *mcpconf.Upstream {
	return &mcpconf.Upstream{
		Name: "grafana", Image: image, Port: 8000,
		Args:      []string{"-t", "streamable-http", "--address=127.0.0.1:8000"},
		Env:       []string{"GRAFANA_URL=https://grafana.example.com", "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=/run/secrets/grafana/token"},
		TokenFile: "/run/redoma-mcp/grafana/token",
		Egress:    []string{"grafana.example.com"},
	}
}

func TestPodmanArgs(t *testing.T) {
	got := PodmanArgs(grafana())
	want := []string{
		"podman", "run", "--rm", "--replace",
		"--name", "redoma-mcp-grafana",
		"--annotation", "redoma/upstream=grafana",
		"--network", "none",
		"--userns", "auto",
		"--cap-drop", "all",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--pull", "missing",
		"--log-driver", "passthrough",
		"--volume", "/run/redoma-mcp/grafana:/run/secrets/grafana:ro,idmap",
		"--env", "GRAFANA_URL=https://grafana.example.com",
		"--env", "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=/run/secrets/grafana/token",
		"--env", "HTTPS_PROXY=http://127.0.0.1:3128",
		"--env", "NO_PROXY=127.0.0.1,localhost",
		image, "-t", "streamable-http", "--address=127.0.0.1:8000",
	}
	if !slices.Equal(got, want) {
		t.Errorf("PodmanArgs =\n%q\nwant\n%q", got, want)
	}
}

// Without a credential, egress or env: only the container's own flags.
func TestPodmanArgsBare(t *testing.T) {
	u := &mcpconf.Upstream{Name: "x", Image: image, Port: 1}
	got := strings.Join(PodmanArgs(u), " ")
	for _, absent := range []string{"--volume", "--env", "HTTPS_PROXY"} {
		if strings.Contains(got, absent) {
			t.Errorf("%q in %s", absent, got)
		}
	}
	if !strings.HasSuffix(got, image) {
		t.Errorf("the image is not last: %s", got)
	}
}

// An HTTPS_PROXY of the upstream's own is kept, not doubled.
func TestPodmanArgsOwnProxy(t *testing.T) {
	u := grafana()
	u.Env = append(u.Env, "HTTPS_PROXY=http://127.0.0.1:3128")
	n := 0
	for _, a := range PodmanArgs(u) {
		if strings.HasPrefix(a, "HTTPS_PROXY=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d HTTPS_PROXY", n)
	}
}

func TestCheckToken(t *testing.T) {
	dir := t.TempDir()
	u := grafana()
	u.TokenFile = filepath.Join(dir, "token")
	if err := CheckToken(u); err == nil || !strings.Contains(err.Error(), "redoma-mcp-token@grafana") {
		t.Errorf("missing: %v", err)
	}
	if err := os.WriteFile(u.TokenFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckToken(u); err == nil {
		t.Error("empty: no error")
	}
	if err := os.WriteFile(u.TokenFile, []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckToken(u); err != nil {
		t.Error(err)
	}
	u.TokenFile = ""
	if err := CheckToken(u); err != nil {
		t.Errorf("no credential: %v", err)
	}
}

func TestCopyToken(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "minted")
	dest := filepath.Join(dir, "run", "grafana", "token")
	if err := os.WriteFile(src, []byte("token-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyToken(src, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "token-1\n" {
		t.Fatalf("dest %q, %v", got, err)
	}
	for path, mode := range map[string]os.FileMode{dest: 0o600, filepath.Dir(dest): 0o700} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v; want %v", path, fi.Mode().Perm(), err, mode)
		}
	}

	// A missing, empty or blank source keeps the last credential.
	for name, content := range map[string]*string{"missing": nil, "empty": new(string), "blank": ptr(" \n")} {
		os.Remove(src)
		if content != nil {
			if err := os.WriteFile(src, []byte(*content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := CopyToken(src, dest); err == nil {
			t.Errorf("%s: no error", name)
		}
		if got, _ := os.ReadFile(dest); string(got) != "token-1\n" {
			t.Errorf("%s: dest became %q", name, got)
		}
	}
	// A relative source, even one that exists, is refused: the unit's
	// working directory is not the minter's.
	t.Chdir(dir)
	if err := os.WriteFile("relative", []byte("token-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyToken("relative", dest); err == nil {
		t.Error("a relative source: no error")
	}
}

// A directory made by someone else, more open, is made 0700.
func TestCopyTokenTightensTheDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "minted")
	dest := filepath.Join(dir, "run", "token")
	if err := os.WriteFile(src, []byte("t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CopyToken(src, dest); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(dest)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("directory %v, %v; want 0700", fi.Mode().Perm(), err)
	}
}

func ptr(s string) *string { return &s }
