package upstreamrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

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
		"--user", "0:0",
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

// uid is the test's own uid, the minter's in these tests.
var uid = uint32(os.Getuid())

func TestCopyToken(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "minter", "minted")
	dest := filepath.Join(root, "run", "grafana", "token")
	if err := os.Mkdir(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("token-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyToken(root, src, dest, uid); err != nil {
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
		if err := copyToken(root, src, dest, uid); err == nil {
			t.Errorf("%s: no error", name)
		}
		if got, _ := os.ReadFile(dest); string(got) != "token-1\n" {
			t.Errorf("%s: dest became %q", name, got)
		}
	}
	// A relative source, even one that exists, is refused: the unit's
	// working directory is not the minter's.
	t.Chdir(root)
	if err := os.WriteFile("relative", []byte("token-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyToken("relative", dest, uid); err == nil {
		t.Error("a relative source: no error")
	}
}

// The copier is root, and the source a path the minter chooses: it copies
// the minter's own regular file and nothing else, and keeps the last
// credential when it refuses.
func TestCopyTokenTrustsTheSourceNoFurtherThanTheMinter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root, src string) // src is root/minter/minted, written 0644
		owner uint32
		want  string
	}{
		{"another owner", nil, uid + 1, "the minter's"},
		{"a symlink to another file", func(t *testing.T, root, src string) {
			other := filepath.Join(root, "other")
			mustWrite(t, other, "secret-of-root\n", 0o600)
			os.Remove(src)
			if err := os.Symlink(other, src); err != nil {
				t.Fatal(err)
			}
		}, uid, "too many levels of symbolic links"},
		{"a directory swapped for a symlink", func(t *testing.T, root, src string) {
			elsewhere := filepath.Join(root, "elsewhere")
			mustWrite(t, filepath.Join(elsewhere, "minted"), "secret-of-root\n", 0o600)
			if err := os.RemoveAll(filepath.Dir(src)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Dir(src)); err != nil {
				t.Fatal(err)
			}
		}, uid, "never a symlink"},
		{"a group-writable file", func(t *testing.T, root, src string) { chmod(t, src, 0o664) }, uid, "writable by others"},
		{"a world-writable file", func(t *testing.T, root, src string) { chmod(t, src, 0o646) }, uid, "writable by others"},
		{"a group-writable directory", func(t *testing.T, root, src string) { chmod(t, filepath.Dir(src), 0o775) }, uid, "writable by others"},
		{"a sticky world-writable directory not root's", func(t *testing.T, root, src string) { chmod(t, filepath.Dir(src), os.ModeSticky|0o777) }, uid, "writable by others"},
		{"a directory", func(t *testing.T, root, src string) {
			os.Remove(src)
			if err := os.Mkdir(src, 0o755); err != nil {
				t.Fatal(err)
			}
		}, uid, "not a regular file"},
		{"a FIFO", func(t *testing.T, root, src string) {
			os.Remove(src)
			if err := syscall.Mkfifo(src, 0o644); err != nil {
				t.Fatal(err)
			}
		}, uid, "not a regular file"},
		{"too large", func(t *testing.T, root, src string) {
			mustWrite(t, src, strings.Repeat("t", maxToken+1), 0o644)
		}, uid, "over"},
		{"not clean", nil, uid, "clean absolute path"},
		{"outside the root", nil, uid, "not under"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "minter", "minted")
			dest := filepath.Join(root, "run", "token")
			mustWrite(t, src, "token-1\n", 0o644)
			if err := copyToken(root, src, dest, uid); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, src, "token-2\n", 0o644)
			if tc.setup != nil {
				tc.setup(t, root, src)
			}
			switch tc.name {
			case "not clean":
				src = root + "/minter/../minter/minted"
			case "outside the root":
				src = filepath.Join(filepath.Dir(root), "elsewhere", "minted")
			}
			done := make(chan error, 1)
			go func() { done <- copyToken(root, src, dest, tc.owner) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v; want one with %q", err, tc.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the copy blocked")
			}
			if got, _ := os.ReadFile(dest); string(got) != "token-1\n" {
				t.Errorf("dest became %q", got)
			}
		})
	}
}

// A directory on the source's path: root's or the minter's, writable by
// its owner alone; not even a root-owned sticky one, as /tmp is.
func TestSafeDir(t *testing.T) {
	const minter = 1000
	for _, tc := range []struct {
		uid  uint32
		mode uint32
		ok   bool
	}{
		{0, 0o755, true},
		{minter, 0o700, true},
		{minter, 0o750, true},
		{0, syscall.S_ISVTX | 0o777, false},
		{1001, 0o755, false},
		{1001, 0o700, false},
		{minter, 0o775, false},
		{minter, 0o757, false},
		{0, 0o777, false},
		{0, 0o775, false},
		{minter, syscall.S_ISVTX | 0o777, false},
		{1001, syscall.S_ISVTX | 0o777, false},
	} {
		st := &syscall.Stat_t{Uid: tc.uid, Mode: syscall.S_IFDIR | tc.mode}
		if err := safeDir("/d", st, minter); (err == nil) != tc.ok {
			t.Errorf("uid %d mode %o: %v, want ok %v", tc.uid, tc.mode, err, tc.ok)
		}
	}
}

// The file itself must be the minter's (here directly under the root, so
// no directory's check comes first).
func TestOpenSourceFileOwner(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "minted")
	mustWrite(t, src, "t\n", 0o600)
	if _, err := OpenSource(root, src, uid+1); err == nil || !strings.Contains(err.Error(), "not the minter's") {
		t.Errorf("err = %v", err)
	}
	f, err := OpenSource(root, src, uid)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	chmod(t, path, mode)
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// A directory made by someone else, more open, is made 0700.
func TestCopyTokenTightensTheDirectory(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "minted")
	dest := filepath.Join(root, "run", "token")
	if err := os.WriteFile(src, []byte("t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyToken(root, src, dest, uid); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(dest)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("directory %v, %v; want 0700", fi.Mode().Perm(), err)
	}
}

func ptr(s string) *string { return &s }
