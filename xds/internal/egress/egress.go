// Package egress reads an agent's egress allowlist: <etc>/egress/<repo>.txt,
// one host name per line, `#` starting a comment, after the defaults every
// agent gets, <etc>/egress/_default.txt, in the same format. The hosts are
// the ones the agent's CONNECT listener opens tunnels to, on port 443. The
// host's administrator writes the files; this package only reads them.
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
// letters or more. Anything else (a port, a wildcard, a URL) is refused.
var hostName = regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]{2,}$`)

// Path is the allowlist of repo under etcDir.
func Path(etcDir, repo string) string {
	return filepath.Join(etcDir, "egress", repo+".txt")
}

// Hosts returns repo's allowed hosts: the defaults' in file order, then
// repo's own in file order, each host once. A missing file adds no hosts:
// with neither, the agent may tunnel nowhere. A line that is not a host
// name, in either file, fails the whole list, so a typo never widens or
// silently narrows it.
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

// read returns one file's host names in file order; a missing file has none.
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
		if !hostName.MatchString(h) {
			return nil, fmt.Errorf("%s: not a host name: %q", path, h)
		}
		hosts = append(hosts, h)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return hosts, nil
}
