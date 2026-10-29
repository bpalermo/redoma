package modelauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMode(t *testing.T, etc, content string) {
	t.Helper()
	if err := os.WriteFile(Path(etc), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRead(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    Mode
	}{
		{"api-key", APIKey},
		{"api-key\n", APIKey},
		{"bearer", Bearer},
		{"  bearer \n\n", Bearer},
		{"\tbearer\r\n", Bearer},
	} {
		etc := t.TempDir()
		writeMode(t, etc, tc.content)
		got, err := Read(etc)
		if err != nil || got != tc.want {
			t.Errorf("Read(%q) = %v, %v; want %v, nil", tc.content, got, err, tc.want)
		}
	}
}

func TestReadMissingFileIsAPIKey(t *testing.T) {
	got, err := Read(t.TempDir())
	if err != nil || got != APIKey {
		t.Errorf("Read = %v, %v; want api-key, nil", got, err)
	}
}

// TestReadRefusesAnythingElse: an unknown word is None, so the listener
// carries no credential, and the error does not repeat what the file holds.
func TestReadRefusesAnythingElse(t *testing.T) {
	for _, content := range []string{
		"", "\n", "Bearer", "API-KEY", "apikey", "bearer api-key", "bearer\nbearer",
		"none", "Bearer placeholder-token",
	} {
		etc := t.TempDir()
		writeMode(t, etc, content)
		got, err := Read(etc)
		if err == nil || got != None {
			t.Errorf("Read(%q) = %v, %v; want none and an error", content, got, err)
			continue
		}
		if strings.Contains(err.Error(), "placeholder-token") {
			t.Errorf("Read(%q): the error quotes the file: %v", content, err)
		}
	}
}

func TestReadUnreadableIsNone(t *testing.T) {
	etc := t.TempDir()
	// A directory where the file should be: reading it fails whoever runs
	// the test, root included.
	if err := os.Mkdir(filepath.Join(etc, "model-auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Read(etc)
	if err == nil || got != None {
		t.Errorf("Read = %v, %v; want none and an error", got, err)
	}
}
