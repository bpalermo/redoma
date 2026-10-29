// Package allowlist reads /etc/redoma/agents.txt, the names an agent may
// have, the way a launcher reads it with `grep -qxF`. A name is an identity
// (spiffe://<td>/agent/<name>), so whoever can change the file can mint
// identities: a file another user than its owner could have changed allows
// nothing.
package allowlist

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// name is the check of an agent name, the same everywhere it is read.
var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// ValidName reports whether s can be an agent's name.
func ValidName(s string) bool {
	return name.MatchString(s)
}

// File is an allowlist on disk.
type File struct {
	// Path is the file (/etc/redoma/agents.txt): a name is allowed when a
	// line is exactly the name, as `grep -qxF` reads it. Any
	// other line (a comment, an indented name) allows nothing; a line with a
	// carriage return makes the file refused whole (see Allowed).
	Path string
	// Owner is the uid that must own Path and the directories above it: 0,
	// root, unless a test sets its own.
	Owner uint32
	// Root is the highest directory checked above Path: "/" unless a test
	// sets the temporary directory it works in.
	Root string
	Log  *slog.Logger
}

// maxSize bounds the allowlist, a list of names.
const maxSize = 1 << 20

// Allowed reports whether name is a whole line of the file, refusing a file
// another user than Owner could have changed: the file itself (opened
// without following a symlink) and every directory from its own up to Root.
// It reads the file again on every call.
func (a File) Allowed(name string) (bool, error) {
	f, err := os.OpenFile(a.Path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("the allowlist (not a symlink): %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("the allowlist: %w", err)
	}
	if err := a.trusted(a.Path, fi); err != nil {
		return false, err
	}
	if err := a.trustedParents(); err != nil {
		return false, err
	}
	// The whole file is read: a carriage return anywhere refuses it. A CRLF
	// line "name\r" would be "name" to bufio but not to a launcher's
	// grep, and the two must agree on every name; failing closed keeps a
	// file edited on another system from allowing names one reader sees.
	// Read whole, not with bufio.ScanLines, which drops a CR before the LF.
	raw, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return false, fmt.Errorf("the allowlist: %w", err)
	}
	if len(raw) > maxSize {
		return false, fmt.Errorf("the allowlist %s is over %d bytes", a.Path, maxSize)
	}
	text := string(raw)
	if strings.ContainsRune(text, '\r') {
		a.Log.Error("the allowlist has a carriage return (CRLF line ends?); allowing no name", "file", a.Path)
		return false, fmt.Errorf("the allowlist %s has a carriage return; it must have plain LF line ends", a.Path)
	}
	for _, line := range strings.Split(text, "\n") {
		if line == name {
			return true, nil
		}
	}
	return false, nil
}

// trusted refuses a file or directory not owned by Owner, or writable by its
// group or by others.
func (a File) trusted(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != a.Owner || fi.Mode().Perm()&0o022 != 0 {
		a.Log.Error("the allowlist may have been changed by someone else than its owner; allowing no name", "path", path, "mode", fi.Mode())
		return fmt.Errorf("%s must be owned by uid %d and writable by it alone, for the allowlist %s", path, a.Owner, a.Path)
	}
	return nil
}

// trustedParents checks each directory from the allowlist's up to Root: a
// real directory, not a symlink, trusted.
func (a File) trustedParents() error {
	root := a.Root
	if root == "" {
		root = "/"
	}
	root = filepath.Clean(root)
	for dir := filepath.Dir(filepath.Clean(a.Path)); ; dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("the allowlist's directory: %w", err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s, above the allowlist, is not a directory", dir)
		}
		if err := a.trusted(dir, fi); err != nil {
			return err
		}
		if dir == root || dir == filepath.Dir(dir) {
			return nil
		}
	}
}
