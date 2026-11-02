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

	"github.com/bpalermo/redoma/common/atomicfile"
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
// With egress hosts, HTTPS_PROXY points at its egress listener unless its
// env sets one.
func PodmanArgs(u *mcpconf.Upstream) []string {
	args := []string{
		"podman", "run", "--rm", "--replace",
		"--name", ContainerName(u.Name),
		"--annotation", Annotation + "=" + u.Name,
		"--network", "none",
		"--userns", "auto",
		"--cap-drop", "all",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--pull", "missing",
		"--log-driver", "passthrough",
	}
	if u.TokenFile != "" {
		args = append(args, "--volume", filepath.Dir(u.TokenFile)+":"+SecretsDir(u.Name)+":ro,idmap")
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
// writes, to dest (the upstream's token_file), atomically, 0600, making its
// directory 0700. A missing or empty source is an error and leaves dest as
// it was: the server keeps the last credential until it expires.
func CopyToken(source, dest string) error {
	if !filepath.IsAbs(source) {
		return fmt.Errorf("the credential's source %q is not an absolute path", source)
	}
	f, err := os.Open(source)
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
