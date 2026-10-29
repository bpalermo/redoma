package register

import (
	"path/filepath"
	"regexp"
)

// NetnsPrefix starts the name of a hook-registered container's namespace in
// the netns directory: <netns dir>/redoma-oci-<container ID>. It never
// collides with a launcher's redoma-<name>, whose name is at most 41
// characters, and is never reused, since container IDs are not.
const NetnsPrefix = "redoma-oci-"

// containerID is what a container ID may be: exactly 64 hex digits, the
// full ID Podman passes to hooks. No shorter form: redoma-oci-<12 hex> is
// also a launcher's namespace name for the valid name oci-<12 hex>.
var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidContainer reports whether id can be a container ID.
func ValidContainer(id string) bool {
	return containerID.MatchString(id)
}

// NetnsPath is where the hook bind-mounts container's network namespace.
func NetnsPath(netnsDir, container string) string {
	return filepath.Join(netnsDir, NetnsPrefix+container)
}
