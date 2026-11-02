package hook

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bpalermo/redoma/common/forbidden"
)

// ForbiddenSources are the host paths no agent may have mounted, nor
// anything inside them, nor any directory above them (forbidden.Base). The
// command adds the directories of SPIRE's sockets the settings name
// (forbidden.Sources).
var ForbiddenSources = forbidden.Base

// Graphroot and Runroot are Podman's rootful storage and its run state,
// containers-storage.conf's defaults. The hook does not read the host's
// storage.conf: a host that moves them is not covered until these and
// ForbiddenSources follow (AGENTS.md).
const (
	Graphroot = "/var/lib/containers/storage"
	Runroot   = "/run/containers/storage"
)

// mountTypes are the mount types an agent's spec may have besides bind:
// the pseudo file systems Podman sets up, whose source names no host path.
var mountTypes = []string{"proc", "tmpfs", "devpts", "mqueue", "sysfs", "cgroup", "cgroup2"}

// policy is what the mount checks hold a container to.
type policy struct {
	forbidden []string
	// own matches the container's own userdata files that Podman 4.9 binds
	// into it from its graphroot (libpod: ShmDir, the bundle's shm, and the
	// passwd and group it writes for --user; container_internal_common.go
	// makeBindMounts, generatePasswdAndGroup):
	// <graphroot>/<driver>-containers/<its id>/userdata/{shm,passwd,group}.
	// Everything else Podman binds (hosts, resolv.conf, hostname,
	// .containerenv, localtime, run/secrets) is under its runroot,
	// /run/containers, which is not forbidden. Its --secret files
	// (userdata/secrets) and named volumes (<graphroot>/volumes) are not
	// matched: an agent with either is refused.
	own *regexp.Regexp
	// masked are the spec's linux.maskedPaths, whose files the runtime masks
	// by binding the host's /dev/null on them.
	masked []string
	// ownRootfs is the container's own rootfs as its storage records it
	// (containerRootfs), not as its spec says: "" when it is not known.
	ownRootfs string
	// allowed is a directory inside a forbidden path the container may have
	// mounted, it or what is inside it, and nothing above it: an MCP
	// upstream's own credential directory. "" for an agent.
	allowed string
}

// exempt says that p, a host path, is allowed, or inside it, and f, the
// forbidden path it relates to, is above allowed: f is forbidden for what
// it holds besides allowed.
func (pl policy) exempt(p, f string) bool {
	if pl.allowed == "" || !strings.HasPrefix(pl.allowed, f+"/") {
		return false
	}
	return p == pl.allowed || strings.HasPrefix(p, pl.allowed+"/")
}

func newPolicy(forbidden []string, graphroot, container string, masked []string, ownRootfs string) policy {
	return policy{
		ownRootfs: ownRootfs,
		forbidden: forbidden,
		own:       regexp.MustCompile(`^` + regexp.QuoteMeta(filepath.Clean(graphroot)) + `/[a-z0-9]+-containers/` + regexp.QuoteMeta(container) + `/userdata/(shm|passwd|group)$`),
		masked:    masked,
	}
}

// reach is the forbidden path p is, is inside, or holds, if any; "" also
// when p is one of the container's own userdata files.
func (pl policy) reach(p string) string {
	if pl.own.MatchString(p) {
		return ""
	}
	for _, f := range pl.forbidden {
		if related(p, f) && !pl.exempt(p, f) {
			return f
		}
	}
	return ""
}

// related says that a is b, is inside b, or holds b.
func related(a, b string) bool {
	return a == b || a == "/" || b == "/" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// isBind says that a mount is a bind mount: its type, or its options.
func isBind(typ string, options []string) bool {
	return typ == "bind" || slices.Contains(options, "bind") || slices.Contains(options, "rbind")
}

// checkMounts refuses a mount whose source is, is inside, or holds a
// forbidden path, as written and as it resolves on the host (the runtime
// follows a symlinked source); a bind whose source does not resolve (it
// could not be checked); a bind whose source is not an absolute path (the
// runtime takes it relative to the bundle); and a mount of any other type
// than mountTypes.
func (pl policy) checkMounts(top map[string]json.RawMessage) error {
	var mounts []map[string]json.RawMessage
	if err := field(top, "mounts", "spec", &mounts); err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	for i, m := range mounts {
		if err := pl.checkMount(m, fmt.Sprintf("mounts[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

func (pl policy) checkMount(m map[string]json.RawMessage, what string) error {
	var source, typ string
	var options []string
	for key, v := range map[string]any{"source": &source, "type": &typ, "options": &options} {
		if err := field(m, key, what, v); err != nil {
			return fmt.Errorf("the container's spec: %w", err)
		}
	}
	if !isBind(typ, options) {
		if !slices.Contains(mountTypes, typ) {
			return fmt.Errorf("the container mounts %q of type %q: an agent has binds and the runtime's own file systems only", source, typ)
		}
		return nil
	}
	if !filepath.IsAbs(source) {
		return fmt.Errorf("the container binds %q, not an absolute path (the runtime would take it relative to the bundle)", source)
	}
	real, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("the container binds %s, which does not resolve on the host (%v): it cannot be checked", source, err)
	}
	for _, p := range []string{filepath.Clean(source), real} {
		if f := pl.reach(p); f != "" {
			return fmt.Errorf("the container mounts %s, which reaches %s: an agent never sees the host's control plane, SPIRE or Podman", source, f)
		}
	}
	if pl.allowed != "" {
		return pl.checkUpstreamBind(m, what, source, real, options)
	}
	return nil
}

// checkUpstreamBind holds an MCP upstream's bind to its own rules.
//
// An idmapped bind (an idmap or ridmap option, or mappings of the mount's
// own) lets the upstream's root, an unprivileged host id, own what host
// root owns under it: only its credential directory may be one, as written
// and as resolved.
//
// A bind of that directory, or of anything in it, as written or as
// resolved, must be read-only, so the container cannot leave entries where
// the root copier writes. It may carry only credentialOptions, ro or rro
// among them: runc and crun apply options in order, and recursively
// (rrw, with mount_setattr, after the ro remount), so a rule that looks
// for ro and refuses known writable spellings misses the next one.
func (pl policy) checkUpstreamBind(m map[string]json.RawMessage, what, source, real string, options []string) error {
	var uidMappings, gidMappings []json.RawMessage
	if err := field(m, "uidMappings", what, &uidMappings); err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	if err := field(m, "gidMappings", what, &gidMappings); err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	mapped := len(uidMappings) > 0 || len(gidMappings) > 0 || slices.ContainsFunc(options, isIdmapOption)
	clean := filepath.Clean(source)
	if mapped && !(pl.inAllowed(clean) && pl.inAllowed(real)) {
		return fmt.Errorf("the container binds %s idmapped: only an MCP upstream's own credential directory may be", source)
	}
	if !pl.inAllowed(clean) && !pl.inAllowed(real) {
		return nil
	}
	for _, o := range options {
		if !credentialOption(o) {
			return fmt.Errorf("the container binds its credential directory %s with the option %q: only %v (and idmap=...) may be given, so it stays read-only", source, o, credentialOptions)
		}
	}
	if !slices.Contains(options, "ro") && !slices.Contains(options, "rro") {
		return fmt.Errorf("the container binds its credential directory %s writable: it is read-only (ro or rro)", source)
	}
	return nil
}

// credentialOptions are the only options a bind of an upstream's
// credential directory may carry: the bind, read-only, idmap, the
// restrictions, and the propagation Podman 4.9 sets (rprivate).
var credentialOptions = []string{"bind", "rbind", "ro", "rro", "idmap", "nosuid", "nodev", "noexec", "rprivate"}

// credentialOption says that o is one of credentialOptions, or idmap with
// its mappings (idmap=uids=...;gids=...).
func credentialOption(o string) bool {
	return slices.Contains(credentialOptions, o) || strings.HasPrefix(o, "idmap=")
}

// checkNoSubmounts refuses an upstream whose credential directory has a
// file system mounted below it on the host. A read-only rbind makes only
// its top mount read-only (ro is not rro, and Podman's :ro is ro), so a
// submount would be writable in the container: the directory holds plain
// files only. An agent has no such directory.
func (pl policy) checkNoSubmounts(host []mountEntry) error {
	if pl.allowed == "" {
		return nil
	}
	for _, e := range host {
		if strings.HasPrefix(e.point, pl.allowed+"/") {
			return fmt.Errorf("%s has a file system mounted below it, at %s: an MCP upstream's credential directory holds plain files, since a read-only bind of it would leave a submount writable", pl.allowed, e.point)
		}
	}
	return nil
}

// inAllowed says that p is the upstream's credential directory, or in it.
func (pl policy) inAllowed(p string) bool {
	return p == pl.allowed || strings.HasPrefix(p, pl.allowed+"/")
}

// isIdmapOption is a mount option that idmaps the mount: idmap or ridmap,
// with or without its mappings (idmap=uids=...;gids=...).
func isIdmapOption(o string) bool {
	name, _, _ := strings.Cut(o, "=")
	return name == "idmap" || name == "ridmap"
}

// mountEntry is one line of a mountinfo: the device, the path within the
// device's file system that is mounted (root), and where.
type mountEntry struct {
	dev, root, point string
}

// parseMountinfo reads a /proc/<pid>/mountinfo.
func parseMountinfo(text string) []mountEntry {
	var out []mountEntry
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		// id parent major:minor root point options... - fstype source super
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		out = append(out, mountEntry{dev: f[2], root: unescape(f[3]), point: unescape(f[4])})
	}
	return out
}

// unescape undoes mountinfo's octal escapes (\040 for a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// hostEntry is the host mount that holds p: the longest mount point above
// it, the last mounted when several.
func hostEntry(host []mountEntry, p string) (mountEntry, bool) {
	var best mountEntry
	found := false
	for _, e := range host {
		if e.point == p || e.point == "/" || strings.HasPrefix(p, e.point+"/") {
			if !found || len(e.point) >= len(best.point) {
				best, found = e, true
			}
		}
	}
	return best, found
}

// within is p's path in its file system, given the host mount e that
// holds it.
func within(e mountEntry, p string) string {
	rest := strings.TrimPrefix(p, e.point)
	if e.point == "/" {
		rest = p
	}
	return filepath.Clean(e.root + "/" + rest)
}

// place is a forbidden place on the host as mountinfo names it: a device,
// and a path in its file system; every container mount of that device
// whose root is, is inside, or holds it reaches it.
type place struct {
	forbidden string
	host      mountEntry
	rel       string
}

// places are the forbidden places of the host: each forbidden path in the
// file system that holds it, and every file system mounted inside one (a
// separate /var/lib/containers, the tmpfs of another container's shm),
// whole. The container's own rootfs (its overlay, as its storage records
// it) and userdata files (its shm), mounted on the host as file systems of
// their own, are not; any other mount inside a forbidden path is, another
// container's overlay included.
func (pl policy) places(host []mountEntry) []place {
	var out []place
	for _, f := range pl.forbidden {
		if he, ok := hostEntry(host, f); ok {
			out = append(out, place{forbidden: f, host: he, rel: within(he, f)})
		}
		for _, he := range host {
			if strings.HasPrefix(he.point, f+"/") && (pl.ownRootfs == "" || he.point != pl.ownRootfs) && !pl.own.MatchString(he.point) {
				out = append(out, place{forbidden: f, host: he, rel: he.root})
			}
		}
	}
	return out
}

// checkMountinfo compares what the container has mounted, read from its
// mountinfo at createRuntime, with the forbidden places on the host: a
// container mount of a forbidden place's device whose root is, is inside,
// or holds its path there is refused. This catches what the spec's paths
// do not show: a bind from another name of the same place (a second bind
// of /run elsewhere, /var/run), and a symlink swapped between the check
// above and the runtime's mount. The runtime's masks (the host's /dev/null
// bound on each of the spec's maskedPaths) are not refused.
//
// At createRuntime the runtime has made the container's mounts under its
// rootfs but not yet pivoted into it, so the container's namespace still
// holds a copy of every host mount: only those under rootfs are the
// container's (ownMounts).
func (pl policy) checkMountinfo(host, ctr []mountEntry, rootfs string) error {
	own := ownMounts(host, ctr, rootfs)
	nullDev := ""
	if he, ok := hostEntry(host, "/dev/null"); ok && within(he, "/dev/null") == "/null" {
		nullDev = he.dev
	}
	for _, p := range pl.places(host) {
		for _, c := range own {
			if pl.reaches(c, p, nullDev, rootfs) {
				return fmt.Errorf("the container has %s mounted at %s, which reaches %s", c.root, c.point, p.forbidden)
			}
		}
	}
	return nil
}

// reaches says that the container's mount c is of place p's device and is,
// is inside, or holds its path there; the runtime's masks (the host's
// /dev/null on a maskedPath) and the container's own userdata files, seen
// from the host, do not count.
func (pl policy) reaches(c mountEntry, p place, nullDev, rootfs string) bool {
	if c.dev != p.host.dev || !related(c.root, p.rel) {
		return false
	}
	if c.dev == nullDev && c.root == "/null" && pl.isMasked(c.point, rootfs) {
		return false
	}
	hostPath := filepath.Clean(p.host.point + "/" + strings.TrimPrefix(c.root, p.host.root))
	return !pl.own.MatchString(hostPath) && !pl.exempt(hostPath, p.forbidden)
}

// isMasked says that point, in the container (under rootfs before the
// pivot), is one of the spec's maskedPaths.
func (pl policy) isMasked(point, rootfs string) bool {
	if rest, ok := strings.CutPrefix(point, rootfs); ok && strings.HasPrefix(rest, "/") {
		point = rest
	}
	return slices.Contains(pl.masked, point)
}

// ownMounts are the container's mounts of its namespace's: those under its
// rootfs; all of them once it has pivoted (its / is not the host's); none
// while it still has the host's / and nothing under its rootfs (a runtime
// that runs the hook before it mounts: the spec check is then what holds).
func ownMounts(host, ctr []mountEntry, rootfs string) []mountEntry {
	var own []mountEntry
	for _, c := range ctr {
		if c.point == rootfs || strings.HasPrefix(c.point, rootfs+"/") {
			own = append(own, c)
		}
	}
	if len(own) > 0 {
		return own
	}
	hostRoot, ok := hostEntry(host, "/")
	for _, c := range ctr {
		if ok && c.point == "/" && c.dev == hostRoot.dev && c.root == hostRoot.root {
			return nil
		}
	}
	return ctr
}

// mountinfo reads a process's mountinfo ("self" for the hook's, the
// host's).
func mountinfo(pid string) ([]mountEntry, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "mountinfo"))
	if err != nil {
		return nil, err
	}
	return parseMountinfo(string(raw)), nil
}

// containerRootfs is the rootfs Podman's storage gives the container id:
// the overlay of its layer, <graphroot>/overlay/<layer>/merged (resolved),
// as one of its records says: containers.json, or volatile-containers.json
// for a container whose record is volatile (podman 4.9 sets it for --rm;
// containers/storage v1.51 then writes it apart), under
// <graphroot>/overlay-containers, or under <runroot>/overlay-containers
// with transient_store. "" when no record has the id with a layer, or the
// driver is not overlay: no rootfs is then the container's own.
func containerRootfs(graphroot, runroot, id string) string {
	for _, dir := range []string{graphroot, runroot} {
		for _, name := range []string{"containers.json", "volatile-containers.json"} {
			if layer := recordedLayer(filepath.Join(dir, "overlay-containers", name), id); layer != "" {
				p := filepath.Join(graphroot, "overlay", layer, "merged")
				if real, err := filepath.EvalSymlinks(p); err == nil {
					return real
				}
				return p
			}
		}
	}
	return ""
}

// recordedLayer is the layer a storage record file gives id, if any.
func recordedLayer(path, id string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var containers []struct {
		ID    string `json:"id"`
		Layer string `json:"layer"`
	}
	if json.Unmarshal(raw, &containers) != nil {
		return ""
	}
	for _, c := range containers {
		if c.ID == id && layerID.MatchString(c.Layer) {
			return c.Layer
		}
	}
	return ""
}

// layerID is what a storage layer's id is.
var layerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
