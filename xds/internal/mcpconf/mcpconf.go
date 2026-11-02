// Package mcpconf reads the host's MCP upstreams' configuration
// (docs/mcp-upstreams.md, "Configuration"):
//
//	<etc>/mcp/<upstream>.conf           the upstream: its image, port,
//	                                    arguments, credential file, egress,
//	                                    the headers removed from agents'
//	                                    requests
//	<etc>/mcp/<upstream>/_default.conf  every agent's tools, first
//	<etc>/mcp/<upstream>/<agent>.conf   one agent's tools; without it the
//	                                    agent has no route to the upstream
//
// Every file is root's and writable by no one else, like agents.txt, and
// strict: an unknown key, a key given twice (list keys excepted), or a
// value that does not parse refuses the whole file. The package names no
// upstream and no tool: what an upstream is lives in its files.
package mcpconf

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/bpalermo/redoma/common/allowlist"
)

// Default names the defaults' allowlist of an upstream. It can never be an
// agent's name (allowlist.ValidName refuses the underscore).
const Default = "_default"

// maxSize bounds a configuration file.
const maxSize = 1 << 20

// toolName is what a tool name may be: MCP's tool names, letters, digits,
// '_', '-' and '.'.
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Dir is the configuration directory: Etc is /etc/redoma, and Owner the uid
// that must own every file and directory read, 0 unless a test sets its own.
type Dir struct {
	Etc   string
	Owner uint32
}

// UpstreamPath is upstream's own file, <etc>/mcp/<upstream>.conf.
func (d Dir) UpstreamPath(upstream string) string {
	return filepath.Join(d.Etc, "mcp", upstream+".conf")
}

// AllowlistPath is agent's allowlist for upstream,
// <etc>/mcp/<upstream>/<agent>.conf (Default for the defaults').
func (d Dir) AllowlistPath(upstream, agent string) string {
	return filepath.Join(d.Etc, "mcp", upstream, agent+".conf")
}

// Upstreams returns the name of every <etc>/mcp/*.conf, sorted, valid or
// not: a bad name is ReadUpstream's to report. Without <etc>/mcp there is
// none, and no error: the directory is the host's switch.
func (d Dir) Upstreams() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(d.Etc, "mcp"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".conf"); ok && !e.IsDir() {
			names = append(names, name)
		}
	}
	return names, nil
}

// ValidUpstream reports whether s can be an upstream's name: an agent
// name's rule, so it fits the same Envoy names and paths.
func ValidUpstream(s string) bool {
	return allowlist.ValidName(s)
}

// line is one `key = value` line of a file, with its number.
type line struct {
	n          int
	key, value string
}

// trailingComment is a value with what reads like a trailing comment: a #
// after a space or tab.
var trailingComment = regexp.MustCompile(`(^|[ \t])#`)

// readLines reads a trusted file into its lines: blank lines and lines
// starting with # skipped, `[section]` lines kept with an empty value. A
// # is a comment only at the start of a line: a value that starts with #,
// or has ` #` in it, is refused, not passed on with the comment. A missing file is nil lines and
// fs.ErrNotExist.
func (d Dir) readLines(path string) ([]line, error) {
	f, err := d.open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(raw) > maxSize {
		return nil, fmt.Errorf("%s: over %d bytes", path, maxSize)
	}
	if strings.ContainsRune(string(raw), '\r') {
		return nil, fmt.Errorf("%s: a carriage return; the file must have plain LF line ends", path)
	}
	var lines []line
	for i, text := range strings.Split(string(raw), "\n") {
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "[") {
			lines = append(lines, line{n: i + 1, key: text})
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" {
			return nil, fmt.Errorf("%s:%d: not `key = value`", path, i+1)
		}
		if trailingComment.MatchString(value) {
			return nil, fmt.Errorf("%s:%d: a # after a space in the value; # starts a comment only at the start of a line", path, i+1)
		}
		lines = append(lines, line{n: i + 1, key: key, value: value})
	}
	return lines, nil
}

// open opens a trusted file. It checks the path before opening it, so a
// FIFO or device in a directory someone else could write is refused, not
// opened, and opens it without following a symlink and without blocking;
// the file opened must be the one checked.
func (d Dir) open(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := d.trusted(path, before); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err == nil && !os.SameFile(before, after) {
		err = fmt.Errorf("%s changed while it was read", path)
	}
	if err == nil {
		err = d.owned(path, after)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// trusted refuses a file another user than Owner could have changed: the
// file itself (fi, from Lstat: a symlink is not a regular file), and every
// directory from its own up to Etc.
func (d Dir) trusted(path string, fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", path)
	}
	if err := d.owned(path, fi); err != nil {
		return err
	}
	etc := filepath.Clean(d.Etc)
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s: not a directory", dir)
		}
		if err := d.owned(dir, fi); err != nil {
			return err
		}
		if dir == etc || dir == filepath.Dir(dir) {
			return nil
		}
	}
}

func (d Dir) owned(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != d.Owner || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be owned by uid %d and writable by it alone (mode %v)", path, d.Owner, fi.Mode().Perm())
	}
	return nil
}
