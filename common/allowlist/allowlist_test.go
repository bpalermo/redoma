package allowlist

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFile(t *testing.T, content string) File {
	t.Helper()
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc")
	if err := os.Mkdir(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(etc, "agents.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return File{Path: path, Owner: uint32(os.Getuid()), Root: dir, Log: slog.New(slog.DiscardHandler)}
}

// TestWholeLines: a name is allowed when a line is exactly the name, the
// launcher's grep -qxF, and a carriage return anywhere refuses every name,
// since the launcher's grep and this reader would not agree on such a line.
func TestWholeLines(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		allowed bool
	}{
		"LF":                   {"beta\nalpha\n", true},
		"no final newline":     {"beta", true},
		"a comment":            {"# beta\n", false},
		"indented":             {"  beta\n", false},
		"annotated":            {"beta # note\n", false},
		"CRLF":                 {"beta\r\n", false},
		"CRLF on another line": {"alpha\r\nbeta\n", false},
		"CR at the end":        {"beta\n\r", false},
		"name not on a line":   {"betas\n", false},
	} {
		got, err := newFile(t, tc.content).Allowed("beta")
		if got != tc.allowed {
			t.Errorf("%s: allowed = %v (%v), want %v", name, got, err, tc.allowed)
		}
	}
}

// TestMustBeTheOwners: a file another user could have written allows no
// name, since the name is the identity.
func TestMustBeTheOwners(t *testing.T) {
	f := newFile(t, "beta\n")
	if err := os.Chmod(f.Path, 0o664); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.Allowed("beta"); ok || err == nil || !strings.Contains(err.Error(), "writable by it alone") {
		t.Errorf("group-writable: %v, %v; want refused", ok, err)
	}
	f = newFile(t, "beta\n")
	f.Owner++
	if ok, err := f.Allowed("beta"); ok || err == nil || !strings.Contains(err.Error(), "must be owned by") {
		t.Errorf("another owner: %v, %v; want refused", ok, err)
	}
}

// TestPathMustBeTrusted: a symlinked file, and one under a directory another
// user could write, allow no name.
func TestPathMustBeTrusted(t *testing.T) {
	f := newFile(t, "beta\n")
	real := f.Path + ".real"
	if err := os.Rename(f.Path, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, f.Path); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.Allowed("beta"); ok || err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Errorf("symlinked: %v, %v; want refused", ok, err)
	}

	f = newFile(t, "beta\n")
	dir := filepath.Dir(f.Path)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.Allowed("beta"); ok || err == nil || !strings.Contains(err.Error(), dir+" must be owned") {
		t.Errorf("world-writable directory: %v, %v; want refused", ok, err)
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"beta", "a", "alpha"} {
		if !ValidName(n) {
			t.Errorf("%q: not valid", n)
		}
	}
	for _, n := range []string{"", "-a", "A", "a/b", "a.b", "a_b", strings.Repeat("a", 42)} {
		if ValidName(n) {
			t.Errorf("%q: valid", n)
		}
	}
}
