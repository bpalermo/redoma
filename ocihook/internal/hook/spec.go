package hook

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
)

// defaultDevices are the device nodes an agent may have: the ones every
// runtime makes for a container anyway (null, zero, full, random, urandom,
// tty, ptmx). Any other (a disk, /dev/kmsg, /dev/fuse) is the host's.
var defaultDevices = map[string][2]int64{
	"/dev/null":    {1, 3},
	"/dev/zero":    {1, 5},
	"/dev/full":    {1, 7},
	"/dev/random":  {1, 8},
	"/dev/urandom": {1, 9},
	"/dev/tty":     {5, 0},
	"/dev/ptmx":    {5, 2},
}

// checkDevices refuses a linux.devices entry that is not one of
// defaultDevices, as a character device of its number.
func checkDevices(top map[string]json.RawMessage) error {
	linux, err := object(top["linux"], "linux")
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	var devices []map[string]json.RawMessage
	if err := field(linux, "devices", "linux", &devices); err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	for i, d := range devices {
		var path, typ string
		var major, minor int64
		what := fmt.Sprintf("linux.devices[%d]", i)
		for key, v := range map[string]any{"path": &path, "type": &typ, "major": &major, "minor": &minor} {
			if err := field(d, key, what, v); err != nil {
				return fmt.Errorf("the container's spec: %w", err)
			}
		}
		if want, ok := defaultDevices[path]; !ok || typ != "c" || major != want[0] || minor != want[1] {
			return fmt.Errorf("the container has the device %s (%s %d:%d) (--device): an agent has the runtime's default devices only", path, typ, major, minor)
		}
	}
	return nil
}

// specRoot is the container's rootfs, absolute (the runtime takes a
// relative one against the bundle).
func specRoot(top map[string]json.RawMessage, bundle string) (string, error) {
	root, err := object(top["root"], "root")
	if err != nil {
		return "", fmt.Errorf("the container's spec: %w", err)
	}
	var path string
	if err := field(root, "path", "root", &path); err != nil {
		return "", fmt.Errorf("the container's spec: %w", err)
	}
	if path == "" {
		return "", fmt.Errorf("the container's spec has no root.path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(bundle, path)
	}
	// Resolved, as the runtime and the kernel resolve it: a symlinked root
	// is where it points, and the mountinfo shows that path.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("the container's root %s does not resolve on the host (%v): it cannot be checked", path, err)
	}
	return real, nil
}

// storage is the forbidden sources, the graphroot and the runroot, the
// defaults but where a test sets its own.
func (h *Hook) storage() (forbidden []string, graphroot, runroot string) {
	forbidden, graphroot, runroot = h.Forbidden, h.Graphroot, h.Runroot
	if forbidden == nil {
		forbidden = ForbiddenSources
	}
	if graphroot == "" {
		graphroot = Graphroot
	}
	if runroot == "" {
		runroot = Runroot
	}
	return forbidden, graphroot, runroot
}

// checkSpec is the spec's checks past its namespaces: the process, the
// devices, and the mounts, as the spec names them and as the runtime has
// made them (the container's mountinfo).
func (h *Hook) checkSpec(raw []byte, st State) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	if err := h.checkProcess(top); err != nil {
		return err
	}
	if err := checkDevices(top); err != nil {
		return err
	}
	linux, err := object(top["linux"], "linux")
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	var masked []string
	if err := field(linux, "maskedPaths", "linux", &masked); err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	if err := checkDeviceRules(linux); err != nil {
		return err
	}
	forbidden, graphroot, runroot := h.storage()
	pl := newPolicy(forbidden, graphroot, st.ID, masked, containerRootfs(graphroot, runroot, st.ID))
	if err := pl.checkMounts(top); err != nil {
		return err
	}
	rootfs, err := specRoot(top, st.Bundle)
	if err != nil {
		return err
	}
	// A root that is not the container's own (--rootfs another container's
	// merged dir, the host's /dev/shm) is one more mount of what it points
	// at.
	if f := pl.reach(rootfs); f != "" && rootfs != pl.ownRootfs {
		return fmt.Errorf("the container's root %s is not its own (as the storage under %s and %s records it) and reaches %s", rootfs, graphroot, runroot, f)
	}
	read := h.Mountinfo
	if read == nil {
		read = mountinfo
	}
	host, err := read("self")
	if err != nil {
		return fmt.Errorf("the host's mounts: %w", err)
	}
	ctr, err := read(strconv.Itoa(st.Pid))
	if err != nil {
		return fmt.Errorf("the container's mounts: %w", err)
	}
	return pl.checkMountinfo(host, ctr, rootfs)
}

// defaultDeviceRules are the cgroup device rules a container may have:
// Podman 4.9's (crun's) own, which deny everything and allow the default
// devices (defaultDevices) and the pseudo-terminals (/dev/pts, char 136).
var defaultDeviceRules = map[string]bool{
	"c 1:3": true, "c 1:5": true, "c 1:7": true, "c 1:8": true, "c 1:9": true,
	"c 5:0": true, "c 5:1": true, "c 5:2": true, "c 136:*": true,
}

// checkDeviceRules refuses a linux.resources.devices allow rule beyond the
// defaults: a deny is always fine, an allow only of one default device.
func checkDeviceRules(linux map[string]json.RawMessage) error {
	resources, err := object(linux["resources"], "linux.resources")
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	var rules []map[string]json.RawMessage
	if err := field(resources, "devices", "linux.resources", &rules); err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	for i, r := range rules {
		var allow bool
		var typ string
		var major, minor *int64
		what := fmt.Sprintf("linux.resources.devices[%d]", i)
		for key, v := range map[string]any{"allow": &allow, "type": &typ, "major": &major, "minor": &minor} {
			if err := field(r, key, what, v); err != nil {
				return fmt.Errorf("the container's spec: %w", err)
			}
		}
		if !allow {
			continue
		}
		if key := deviceRule(typ, major, minor); !defaultDeviceRules[key] {
			return fmt.Errorf("the container's cgroup allows the devices %s (--device, --device-cgroup-rule): an agent has the runtime's default devices only", key)
		}
	}
	return nil
}

// deviceRule names a rule as "<type> <major>:<minor>", * for any.
func deviceRule(typ string, major, minor *int64) string {
	if typ == "" {
		typ = "a"
	}
	num := func(n *int64) string {
		if n == nil || *n < 0 {
			return "*"
		}
		return strconv.FormatInt(*n, 10)
	}
	return typ + " " + num(major) + ":" + num(minor)
}
