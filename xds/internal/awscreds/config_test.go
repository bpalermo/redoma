package awscreds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, id, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "aws"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir, id), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

var good = "# the example agent's role\n" +
	"trust_anchor_arn = " + anchor + "\n" +
	"profile_arn=" + profile + "\n\n" +
	"  role_arn   =   " + role + "  \n"

func TestRead(t *testing.T) {
	dir := t.TempDir()
	p, err := Read(dir, "a")
	if p != nil || err != nil {
		t.Fatalf("no file: %+v, %v; want nil, nil", p, err)
	}
	write(t, dir, "a", good)
	p, err = Read(dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	if p.TrustAnchorARN != anchor || p.ProfileARN != profile || p.RoleARN != role || p.Duration != 0 {
		t.Errorf("%+v", p)
	}
	write(t, dir, "a", good+"session_duration = 90m\n")
	if p, err = Read(dir, "a"); err != nil || p.Duration != 90*time.Minute {
		t.Errorf("%+v, %v", p, err)
	}
}

func TestReadRefuses(t *testing.T) {
	cases := map[string]string{
		"unknown key":            good + "region = us-east-1\n",
		"key twice":              good + "role_arn = " + other + "\n",
		"no role":                strings.Replace(good, "role_arn", "# role_arn", 1),
		"no anchor":              strings.Replace(good, "trust_anchor_arn", "# trust_anchor_arn", 1),
		"no equals":              good + "session_duration 1h\n",
		"bad duration":           good + "session_duration = 1 hour\n",
		"zero duration":          good + "session_duration = 0s\n",
		"duration too long":      good + "session_duration = 13h\n",
		"bad role":               strings.Replace(good, role, "example-agent", 1),
		"quoted value":           strings.Replace(good, role, `"`+role+`"`, 1),
		"profile from elsewhere": strings.Replace(good, profile, strings.Replace(profile, "us-east-1", "eu-west-1", 1), 1),
		"empty file":             "",
	}
	for name, content := range cases {
		dir := t.TempDir()
		write(t, dir, "a", content)
		if p, err := Read(dir, "a"); err == nil {
			t.Errorf("%s: %+v", name, p)
		}
	}
}
