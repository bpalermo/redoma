// Package upstreamrun is what the units of a host MCP upstream run
// (docs/mcp-upstreams.md): the Podman command line of its container, from
// its <etc>/mcp/<upstream>.conf, and the copy of its credential into
// /run/redoma-mcp/<upstream>/.
package upstreamrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/bpalermo/redoma/common/atomicfile"
	"github.com/bpalermo/redoma/common/register"
	"github.com/bpalermo/redoma/xds/internal/mcpconf"
)

// Annotation marks the container as the upstream's, for the OCI hook.
const Annotation = "redoma/upstream"

// ContainerName is the upstream's container.
func ContainerName(upstream string) string {
	return "redoma-mcp-" + upstream
}

// SecretsDir is where the credential's directory is mounted in the
// container: the path its .conf's arg or env lines name.
func SecretsDir(upstream string) string {
	return "/run/secrets/" + upstream
}

// proxy is the upstream's egress listener, in its own namespace.
const proxy = "http://127.0.0.1:3128"

// PodmanArgs is the command line that runs u's container, Podman's argv[0]
// first. The container is the hook's to check: a user namespace of its own
// (--userns=auto), no network but what Envoy binds in it (--network none),
// no capability, no privilege to gain, a read-only root; its credential's
// directory, if it has one, read-only and idmapped into its user namespace.
// The server runs as that namespace's root (--user 0:0, an unprivileged
// host uid), whatever USER its image names: the idmapped 0600 token reads
// as that root's alone.
// With egress hosts, HTTPS_PROXY points at its egress listener unless its
// env sets one.
func PodmanArgs(u *mcpconf.Upstream) []string {
	args := []string{
		"podman", "run", "--rm", "--replace",
		"--name", ContainerName(u.Name),
		"--annotation", Annotation + "=" + u.Name,
		"--network", "none",
		"--userns", "auto",
		"--user", "0:0",
		"--cap-drop", "all",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--pull", "missing",
		"--log-driver", "passthrough",
	}
	if u.TokenFile != "" {
		args = append(args, "--volume", filepath.Dir(u.TokenFile)+":"+SecretsDir(u.Name)+":"+register.UpstreamCredentialVolumeOptions)
	}
	for _, e := range u.Env {
		args = append(args, "--env", e)
	}
	if len(u.Egress) > 0 && !setsEnv(u.Env, "HTTPS_PROXY") {
		args = append(args, "--env", "HTTPS_PROXY="+proxy, "--env", "NO_PROXY=127.0.0.1,localhost")
	}
	args = append(args, u.Image)
	return append(args, u.Args...)
}

func setsEnv(env []string, name string) bool {
	return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, name+"=") })
}

// CheckToken refuses to start an upstream whose credential file is
// missing or empty: it would start, and fail every call.
func CheckToken(u *mcpconf.Upstream) error {
	if u.TokenFile == "" {
		return nil
	}
	fi, err := os.Stat(u.TokenFile)
	if err != nil {
		return fmt.Errorf("upstream %s's credential: %w (redoma-mcp-token@%s copies it)", u.Name, err, u.Name)
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return fmt.Errorf("upstream %s's credential %s is not a non-empty file", u.Name, u.TokenFile)
	}
	return nil
}

// maxToken bounds a credential file.
const maxToken = 64 << 10

// CopyToken copies the credential at source, which the host's minter
// (owner, its uid) writes, to dest (the upstream's token_file), atomically,
// 0600, making its directory 0700. The copier is root and the source a
// path the minter chooses, so it trusts the source no further than the
// minter (OpenSource). A missing or empty source is an error and leaves
// dest as it was: the server keeps the last credential until it expires.
func CopyToken(source, dest string, owner uint32) error {
	return copyToken("/", source, dest, owner)
}

// copyToken is CopyToken with the walk to source starting at root, a
// directory trusted as it is ("/" but in tests).
func copyToken(root, source, dest string, owner uint32) error {
	f, err := OpenSource(root, source, owner)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil {
		return err
	}
	switch {
	case len(raw) > maxToken:
		return fmt.Errorf("%s is over %d bytes", source, maxToken)
	case len(strings.TrimSpace(string(raw))) == 0:
		return errors.New(source + " is empty")
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(dest, raw, 0o600)
}

// OpenSource opens the credential's source, path, under root, refusing
// any file but the minter's own:
//
//   - it walks from root one component at a time, never following a
//     symlink (openat, O_NOFOLLOW), so swapping a directory on the path
//     for a symlink gets a refusal, not another file;
//   - every directory below root must be owned by root or owner and
//     writable by no one else (so not /tmp: a minter writes under its own
//     directories, such as its runtime directory);
//   - the file is opened with O_NOFOLLOW and O_NONBLOCK (a FIFO cannot
//     hang the copy), and must be a regular file owned by owner, writable
//     by no one else, and at most 64 KiB.
func OpenSource(root, path string, owner uint32) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("the credential's source %q is not a clean absolute path", path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("the credential's source %q is not under %s", path, root)
	}
	parts := strings.Split(rel, "/")
	dirfd, err := openDirs(root, parts[:len(parts)-1], owner)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Openat(dirfd, parts[len(parts)-1], syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	syscall.Close(dirfd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if err := safeSource(path, fd, owner); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// openDirs opens root, then each of dirs below it in turn, without
// following a symlink, each one safeDir; it returns the last one's fd.
func openDirs(root string, dirs []string, owner uint32) (int, error) {
	dirfd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("%s: %w", root, err)
	}
	walked := root
	for _, dir := range dirs {
		walked = filepath.Join(walked, dir)
		next, err := syscall.Openat(dirfd, dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(dirfd)
		if err != nil {
			return -1, fmt.Errorf("%s: %w (a directory on the credential's path, never a symlink)", walked, err)
		}
		dirfd = next
		var st syscall.Stat_t
		if err = syscall.Fstat(dirfd, &st); err == nil {
			err = safeDir(walked, &st, owner)
		}
		if err != nil {
			syscall.Close(dirfd)
			return -1, err
		}
	}
	return dirfd, nil
}

// safeSource refuses a source that is not the minter's own regular file,
// writable by it alone (copyToken reads at most maxToken bytes of it).
func safeSource(path string, fd int, owner uint32) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG:
		return fmt.Errorf("%s is not a regular file", path)
	case st.Uid != owner:
		return fmt.Errorf("%s is owned by uid %d, not the minter's %d", path, st.Uid, owner)
	case st.Mode&0o022 != 0:
		return fmt.Errorf("%s is writable by others than its owner (mode %o)", path, st.Mode&0o777)
	}
	return nil
}

// safeDir refuses a directory on the source's path that someone other
// than root and owner could change.
func safeDir(path string, st *syscall.Stat_t, owner uint32) error {
	if st.Uid != 0 && st.Uid != owner {
		return fmt.Errorf("%s is owned by uid %d, neither root nor the minter's %d", path, st.Uid, owner)
	}
	if st.Mode&0o022 != 0 {
		return fmt.Errorf("%s is writable by others than its owner (mode %o)", path, st.Mode&0o7777)
	}
	return nil
}
