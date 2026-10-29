package hook

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ServiceIDs are the host's redoma service identities, by uid and gid. An
// agent container runs in the host's user namespace, so a uid or gid in its
// spec is the host's own, and any of these would give the agent what the
// service has:
//
//   - 961, redoma-sds: redoma-xds's socket (every agent's private key) and
//     SPIRE's admin socket (every agent's identity);
//   - 963, redoma-relay: the relay's socket and SPIRE's Workload API, which
//     attests the relay by its uid;
//   - 964, redoma-lead: the relay's lead socket, from which any agent can
//     be tasked.
var ServiceIDs = map[uint32]string{
	961: "redoma-sds",
	963: "redoma-relay",
	964: "redoma-lead",
}

// AllowedCaps are the only capabilities an agent's process may hold, in any
// of its sets. Every other one can reach past the uid and gid checks: the
// DAC and owner overrides open the service sockets whatever their modes,
// SETUID and SETGID become a service's identity, SYS_ADMIN and SYS_PTRACE
// enter other processes and namespaces, and the rest change the host.
var AllowedCaps = []string{"CAP_KILL", "CAP_NET_BIND_SERVICE", "CAP_AUDIT_WRITE"}

// process is the spec's process: its user, capabilities and
// noNewPrivileges.
type process struct {
	uid, gid        uint32
	additional      []uint32
	hasUser         bool
	noNewPrivileges bool
	caps            map[string][]string
}

// object decodes raw, if set, as a JSON object.
func object(raw json.RawMessage, what string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if raw == nil {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return m, nil
}

// field decodes obj[key], if present, into v.
func field(obj map[string]json.RawMessage, key, what string, v any) error {
	r, ok := obj[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(r, v); err != nil {
		return fmt.Errorf("%s.%s: %w", what, key, err)
	}
	return nil
}

// specProcess reads process from a spec (whose keys noRepeatedKeys has
// checked), with exact keys as the runtime reads them.
func specProcess(top map[string]json.RawMessage) (process, error) {
	p := process{caps: map[string][]string{}}
	proc, err := object(top["process"], "process")
	if err != nil {
		return p, err
	}
	user, err := object(proc["user"], "process.user")
	if err != nil {
		return p, err
	}
	_, hasUID := user["uid"]
	p.hasUser = hasUID
	for key, v := range map[string]any{"uid": &p.uid, "gid": &p.gid, "additionalGids": &p.additional} {
		if err := field(user, key, "process.user", v); err != nil {
			return p, err
		}
	}
	if err := field(proc, "noNewPrivileges", "process", &p.noNewPrivileges); err != nil {
		return p, err
	}
	caps, err := object(proc["capabilities"], "process.capabilities")
	if err != nil {
		return p, err
	}
	for _, set := range []string{"bounding", "effective", "inheritable", "permitted", "ambient"} {
		var names []string
		if err := field(caps, set, "process.capabilities", &names); err != nil {
			return p, err
		}
		p.caps[set] = names
	}
	return p, nil
}

// checkProcess refuses a process that could become, or act as, a host
// service: root (uid 0 in the host's user namespace), a service's uid or
// gid (--user, --group-add), privileges it may gain (no noNewPrivileges),
// or a capability not in AllowedCaps in any set (no --cap-drop all).
func (h *Hook) checkProcess(top map[string]json.RawMessage) error {
	ids := h.ServiceIDs
	if ids == nil {
		ids = ServiceIDs
	}
	p, err := specProcess(top)
	if err != nil {
		return fmt.Errorf("the container's spec: %w", err)
	}
	switch {
	case !p.hasUser || p.uid == 0:
		return fmt.Errorf("the container runs as root (uid %d): an agent runs as a user (--user), never as root in the host's user namespace", p.uid)
	case !p.noNewPrivileges:
		return fmt.Errorf("the container may gain privileges: an agent runs with --security-opt no-new-privileges")
	}
	if err := checkIDs(p, ids); err != nil {
		return err
	}
	return checkCaps(p)
}

// checkIDs refuses a service's uid, a service's gid, or root's group.
func checkIDs(p process, ids map[uint32]string) error {
	if name, ok := ids[p.uid]; ok {
		return fmt.Errorf("the container runs as uid %d (%s), a service's: an agent never has a host service's identity", p.uid, name)
	}
	for _, g := range append([]uint32{p.gid}, p.additional...) {
		if g == 0 {
			return fmt.Errorf("the container has gid 0 (root's group, --user or --group-add): an agent never has it")
		}
		if name, ok := ids[g]; ok {
			return fmt.Errorf("the container has gid %d (%s, --user or --group-add), a service's: an agent never has a host service's identity", g, name)
		}
	}
	return nil
}

// checkCaps refuses a capability not in AllowedCaps in any set.
func checkCaps(p process) error {
	for _, set := range []string{"bounding", "effective", "inheritable", "permitted", "ambient"} {
		for _, c := range p.caps[set] {
			if !slices.Contains(AllowedCaps, c) {
				return fmt.Errorf("the container has %s in its %s capabilities: an agent runs with --cap-drop all (at most %s)", c, set, strings.Join(AllowedCaps, ", "))
			}
		}
	}
	return nil
}
