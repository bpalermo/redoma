// Package forbidden is the list of host paths no agent container may have
// mounted, which the OCI hook enforces (hook.ForbiddenSources) and redoma
// xds checks SPIRE's sockets against before it uses the Broker API.
package forbidden

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/bpalermo/redoma/common/config"
)

// Base are the host paths no agent may have mounted, nor anything inside
// them, nor any directory above them (which would hold them): the control
// plane's and SPIRE's sockets, the launcher's state and configuration,
// Podman's own, from which a container could start another one or read
// another's files (named volumes, --secret files), and the kernel's views of
// other processes, descriptors and devices, which lead anywhere.
//
// /run/redoma holds every runtime directory of redoma's and its host's:
// xds/ (the xDS and registration sockets), relay/ (the agents' socket),
// lead/ (the lead's socket), names/ (the agents' name files), envoy/
// (Envoy's admin socket) and a launcher's active/. One entry forbids them
// all, and any added later.
var Base = []string{
	"/run/redoma",
	"/run/redoma-spire",
	"/etc/redoma",
	"/run/podman",
	"/var/lib/containers",
	"/proc",
	"/dev",
}

// Sources are Base and the directory of each of SPIRE's sockets the
// settings name (Config.SpireSockets), as written and as it resolves
// through symlinks now: a host that moves a socket out of /run/redoma-spire
// keeps it out of the agents' reach. A directory that does not resolve is
// forbidden as written only.
func Sources(c config.Config) []string {
	out := slices.Clone(Base)
	for _, socket := range c.SpireSockets() {
		dir := filepath.Dir(filepath.Clean(socket))
		out = append(out, dir)
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			out = append(out, real)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Under reports whether path is, or is inside, one of sources.
func Under(path string, sources []string) bool {
	path = filepath.Clean(path)
	for _, s := range sources {
		s = filepath.Clean(s)
		if path == s || strings.HasPrefix(path, strings.TrimSuffix(s, "/")+"/") {
			return true
		}
	}
	return false
}
