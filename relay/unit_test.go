package relay

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
)

// filterLine is one SystemCallFilter= assignment: its names (syscalls and
// @groups) and whether it denies them (a leading ~).
type filterLine struct {
	deny  bool
	names []string
}

// syscallFilter reads a unit's SystemCallFilter= lines in order. An empty
// assignment resets the filter, as systemd does.
func syscallFilter(t *testing.T, unit string) []filterLine {
	t.Helper()
	f, err := os.Open(unit)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []filterLine
	s := bufio.NewScanner(f)
	for s.Scan() {
		v, ok := strings.CutPrefix(strings.TrimSpace(s.Text()), "SystemCallFilter=")
		if !ok {
			continue
		}
		if v == "" {
			lines = nil
			continue
		}
		v, deny := strings.CutPrefix(v, "~")
		lines = append(lines, filterLine{deny: deny, names: strings.Fields(v)})
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// allowed reports whether a syscall, held by the given names (itself and
// every group it is in), passes the filter. As systemd does: the first
// line sets the kind of list (an allow list starts from nothing, a deny
// list from everything), and each later line adds (no ~) or removes (~)
// its names, in order.
func allowed(lines []filterLine, holders []string) bool {
	if len(lines) == 0 {
		return true
	}
	ok := lines[0].deny
	for _, l := range lines {
		if slices.ContainsFunc(l.names, func(n string) bool { return slices.Contains(holders, n) }) {
			ok = !l.deny
		}
	}
	return ok
}

// deniedGroups are the groups the unit takes back from @system-service,
// with each syscall in them that the relay makes (strace of its tests,
// which listen, chown and chmod the sockets, write and fsync the queues,
// rename the records, and dial the Workload API) and every group that
// holds that syscall (systemd 255, `systemd-analyze syscall-filter`).
// Whatever else they hold, the relay never calls.
var deniedGroups = map[string]map[string][]string{
	"@privileged": {
		// os.Chown, for each socket's and directory's group.
		"fchownat": {"fchownat", "@chown", "@privileged", "@system-service"},
	},
	"@resources": {},
}

// TestUnitSyscallFilter: the unit's syscall filter lets through what the
// relay does. The relay was killed with SIGSYS at its first chown when the
// unit denied @privileged, which holds @chown.
func TestUnitSyscallFilter(t *testing.T) {
	lines := syscallFilter(t, "redoma-relay.service")
	if len(lines) == 0 || lines[0].deny || !slices.Contains(lines[0].names, "@system-service") {
		t.Fatalf("the filter must start as an allow list of @system-service, got %+v", lines)
	}
	for _, l := range lines {
		if !l.deny {
			continue
		}
		for _, n := range l.names {
			if _, ok := deniedGroups[n]; !ok {
				t.Errorf("the unit denies %s: list the relay's syscalls it holds in deniedGroups (strace the relay's tests, systemd-analyze syscall-filter %s)", n, n)
			}
		}
	}
	for group, syscalls := range deniedGroups {
		for sc, holders := range syscalls {
			if !allowed(lines, holders) {
				t.Errorf("the unit's filter denies %s (in %s), which the relay calls", sc, group)
			}
		}
	}
}

// TestAllowed: later lines win, in either direction.
func TestAllowed(t *testing.T) {
	holders := []string{"fchownat", "@chown", "@privileged", "@system-service"}
	for _, c := range []struct {
		name  string
		lines []filterLine
		want  bool
	}{
		{"none", nil, true},
		{"allow list", []filterLine{{names: []string{"@system-service"}}}, true},
		{"denied after", []filterLine{{names: []string{"@system-service"}}, {deny: true, names: []string{"@privileged", "@resources"}}}, false},
		{"allowed again", []filterLine{{names: []string{"@system-service"}}, {deny: true, names: []string{"@privileged"}}, {names: []string{"@chown"}}}, true},
		{"allowed then denied", []filterLine{{names: []string{"@system-service"}}, {names: []string{"@chown"}}, {deny: true, names: []string{"@privileged"}}}, false},
		{"not in an allow list", []filterLine{{names: []string{"@basic-io"}}}, false},
		{"deny list", []filterLine{{deny: true, names: []string{"@clock"}}}, true},
		{"deny list holding it", []filterLine{{deny: true, names: []string{"fchownat"}}}, false},
	} {
		if got := allowed(c.lines, holders); got != c.want {
			t.Errorf("%s: allowed = %v, want %v", c.name, got, c.want)
		}
	}
}
