// Package egress reads an agent's egress allowlist: <etc>/egress/<repo>.txt,
// one host name or suffix wildcard per line, `#` starting a comment, after
// the defaults every agent gets, <etc>/egress/_default.txt, in the same
// format. The hosts are the ones the agent's CONNECT listener opens tunnels
// to, on port 443. The host's administrator writes the files; this package
// only reads them.
//
// A suffix wildcard, `*.example.com`, allows every name one label or more
// below example.com (a.example.com, a.b.example.com), not example.com
// itself. Only a leading `*.` is a wildcard, and the domain after it must be
// well formed and have two labels or more, so `*.com` is refused. Whether
// that domain is a public suffix (co.uk, a cloud provider's shared domains)
// is not checked: that is the administrator's to judge.
package egress

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Default names the defaults' file, <etc>/egress/_default.txt. It can never
// be an agent's name (allowlist.ValidName refuses the underscore), so no
// agent takes the defaults as its own list.
const Default = "_default"

// hostName is what a host name may be: lower-case labels and a TLD of two
// letters or more. Anything else (a port, a URL) is refused; a line with a
// "*" is a wildcard's to judge (isWildcard).
var hostName = regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]{2,}$`)

// label is one label of a wildcard's domain: lower-case letters, digits and
// inner hyphens, 63 bytes at most; tld is its last label.
var (
	label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tld   = regexp.MustCompile(`^[a-z]{2,63}$`)
)

// wildcardHint is what a line with a "*" that is no wildcard is told.
const wildcardHint = `a wildcard is "*." before a domain of two labels or more, such as *.example.com`

// isWildcard reports whether h is a suffix wildcard: "*." then a domain of
// two labels or more, each a well-formed label, the last a top-level
// domain's.
func isWildcard(h string) bool {
	domain, ok := strings.CutPrefix(h, "*.")
	if !ok || len(domain) > 253 {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 || !tld.MatchString(labels[len(labels)-1]) {
		return false
	}
	for _, l := range labels {
		if !label.MatchString(l) {
			return false
		}
	}
	return true
}

// Path is the allowlist of repo under etcDir.
func Path(etcDir, repo string) string {
	return filepath.Join(etcDir, "egress", repo+".txt")
}

// Hosts returns repo's allowed host names and wildcards: the defaults' in
// file order, then repo's own in file order, each once. A missing file adds
// none: with neither, the agent may tunnel nowhere. A line that is neither a
// host name nor a wildcard, in either file, fails the whole list, so a typo
// never widens or silently narrows it.
func Hosts(etcDir, repo string) ([]string, error) {
	if repo == Default {
		return nil, fmt.Errorf("%q names the defaults' file, not an agent", repo)
	}
	defaults, err := read(Path(etcDir, Default))
	if err != nil {
		return nil, err
	}
	own, err := read(Path(etcDir, repo))
	if err != nil {
		return nil, err
	}
	var hosts []string
	for _, h := range slices.Concat(defaults, own) {
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts, nil
}

// Check returns an error unless h is a line an allowlist accepts: a host
// name or a suffix wildcard. Other files that list egress hosts (an MCP
// upstream's) hold their lines to it too.
func Check(h string) error {
	switch {
	case strings.Contains(h, "*"):
		if !isWildcard(h) {
			return fmt.Errorf("%q: %s", h, wildcardHint)
		}
	case !hostName.MatchString(h):
		return fmt.Errorf("not a host name: %q", h)
	}
	return nil
}

// read returns one file's host names and wildcards in file order; a missing
// file has none.
func read(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var hosts []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		h, _, _ := strings.Cut(scanner.Text(), "#")
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if err := Check(h); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		hosts = append(hosts, h)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return hosts, nil
}
