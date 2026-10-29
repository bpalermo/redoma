package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	for _, content := range []string{"one", "two"} {
		if err := Write(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("read %q, %v; want %q", got, err, content)
		}
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v; want 0600", fi.Mode(), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the file", len(entries))
	}
}
