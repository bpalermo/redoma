package egress

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/common/allowlist"
)

func writeAllowlist(t *testing.T, etc, repo, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(etc, "egress"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(etc, repo), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHosts(t *testing.T) {
	etc := t.TempDir()
	writeAllowlist(t, etc, "a", "# a comment\ngithub.com\n\n  codeload.github.com  # trailing\n")
	got, err := Hosts(etc, "a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github.com", "codeload.github.com"}; !slices.Equal(got, want) {
		t.Errorf("Hosts = %q, want %q", got, want)
	}
}

func TestHostsMissingFileIsNoHosts(t *testing.T) {
	got, err := Hosts(t.TempDir(), "nobody")
	if err != nil || got != nil {
		t.Errorf("Hosts = %q, %v; want nil, nil", got, err)
	}
}

func TestHostsRefusesWhatIsNotAHostName(t *testing.T) {
	for _, line := range []string{"github.com:443", "*.github.com", "https://github.com", "GitHub.com", "localhost"} {
		etc := t.TempDir()
		writeAllowlist(t, etc, "a", "github.com\n"+line+"\n")
		got, err := Hosts(etc, "a")
		if err == nil || !strings.Contains(err.Error(), "not a host name") {
			t.Errorf("%q: Hosts = %q, %v; want a 'not a host name' error", line, got, err)
		}
	}
}

func TestHostsAreTheDefaultsThenTheAgentsOwnEachOnce(t *testing.T) {
	etc := t.TempDir()
	writeAllowlist(t, etc, Default, "# every agent\ncache.example.org\ngithub.com\n")
	writeAllowlist(t, etc, "a", "proxy.golang.org\ngithub.com\nproxy.golang.org\n")
	got, err := Hosts(etc, "a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cache.example.org", "github.com", "proxy.golang.org"}; !slices.Equal(got, want) {
		t.Errorf("Hosts = %q, want %q", got, want)
	}
}

func TestHostsDefaultsAlone(t *testing.T) {
	etc := t.TempDir()
	writeAllowlist(t, etc, Default, "cache.example.org\n")
	got, err := Hosts(etc, "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cache.example.org"}; !slices.Equal(got, want) {
		t.Errorf("Hosts = %q, want %q", got, want)
	}
}

func TestHostsMissingDefaultsIsNoDefaults(t *testing.T) {
	etc := t.TempDir()
	writeAllowlist(t, etc, "a", "github.com\n")
	got, err := Hosts(etc, "a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github.com"}; !slices.Equal(got, want) {
		t.Errorf("Hosts = %q, want %q", got, want)
	}
}

// A bad line in the defaults fails every agent's list, not just the
// defaults: dropping them would silently narrow the list.
func TestHostsRefusesABadDefaultsLine(t *testing.T) {
	etc := t.TempDir()
	writeAllowlist(t, etc, Default, "cache.example.org:443\n")
	writeAllowlist(t, etc, "a", "github.com\n")
	got, err := Hosts(etc, "a")
	if err == nil || !strings.Contains(err.Error(), "_default.txt: not a host name") {
		t.Errorf("Hosts = %q, %v; want a '_default.txt: not a host name' error", got, err)
	}
}

// The defaults' file is no agent's own list: an agent's name can never be
// Default, and Hosts refuses it should one ever reach it.
func TestDefaultIsNoAgentName(t *testing.T) {
	if allowlist.ValidName(Default) {
		t.Fatalf("allowlist.ValidName(%q) = true: an agent could take the defaults' file as its own", Default)
	}
	etc := t.TempDir()
	writeAllowlist(t, etc, Default, "github.com\n")
	if got, err := Hosts(etc, Default); err == nil {
		t.Errorf("Hosts(%q) = %q, nil; want an error", Default, got)
	}
}
