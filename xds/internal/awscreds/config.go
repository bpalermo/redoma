package awscreds

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bpalermo/redoma/xds/internal/rolesanywhere"
)

// Path is the AWS settings of agent id under etcDir: <etc>/aws/<id>.conf.
func Path(etcDir, id string) string {
	return filepath.Join(etcDir, "aws", id+".conf")
}

// The keys of an agent's file.
const (
	keyTrustAnchor = "trust_anchor_arn"
	keyProfile     = "profile_arn"
	keyRole        = "role_arn"
	keyDuration    = "session_duration"
)

// Read returns agent id's Roles Anywhere profile, from Path. A missing file
// is nil and no error: the agent has not opted in. A file is lines of
// `key = value`, blank lines and lines starting with # ignored, with the
// three ARNs and optionally session_duration (a Go duration: 1h, 90m). An
// unknown key, a key given twice, a missing ARN, a line without `=` or a
// value that does not validate (rolesanywhere.Profile.Validate) is an
// error, so a typo never asks for a session of another role.
func Read(etcDir, id string) (*rolesanywhere.Profile, error) {
	path := Path(etcDir, id)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	p, seen, err := parse(path, f)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{keyTrustAnchor, keyProfile, keyRole} {
		if !seen[k] {
			return nil, fmt.Errorf("%s: no %s", path, k)
		}
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// parse reads the file's lines into a profile, and which keys it gave.
func parse(path string, r io.Reader) (*rolesanywhere.Profile, map[string]bool, error) {
	seen := map[string]bool{}
	p := &rolesanywhere.Profile{}
	scanner := bufio.NewScanner(r)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" {
			return nil, nil, fmt.Errorf("%s:%d: not `key = value`", path, n)
		}
		if seen[key] {
			return nil, nil, fmt.Errorf("%s:%d: %s given twice", path, n, key)
		}
		seen[key] = true
		if err := set(p, key, value); err != nil {
			return nil, nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, seen, nil
}

// set gives p the value of one key.
func set(p *rolesanywhere.Profile, key, value string) error {
	switch key {
	case keyTrustAnchor:
		p.TrustAnchorARN = value
	case keyProfile:
		p.ProfileARN = value
	case keyRole:
		p.RoleARN = value
	case keyDuration:
		d, err := time.ParseDuration(value)
		if err != nil || d == 0 {
			return fmt.Errorf("%s %q is not a duration", key, value)
		}
		p.Duration = d
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}
