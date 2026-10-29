package ocihook

import (
	"bytes"
	"errors"
	"maps"
	"os/user"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/ocihook/internal/hook"
)

func fake(groups, users map[string]string, fail error) lookups {
	find := func(m map[string]string, unknown func(string) error) func(string) (string, error) {
		return func(name string) (string, error) {
			if fail != nil {
				return "", fail
			}
			id, ok := m[name]
			if !ok {
				return "", unknown(name)
			}
			return id, nil
		}
	}
	return lookups{
		group: find(groups, func(n string) error { return user.UnknownGroupError(n) }),
		user:  find(users, func(n string) error { return user.UnknownUserError(n) }),
	}
}

// TestServiceIDs: the static ids always; a service renumbered on this host
// adds its id; a missing name is no error; a failed lookup or an id that is
// not a number is logged and keeps the static ids.
func TestServiceIDs(t *testing.T) {
	var log bytes.Buffer
	ids := serviceIDs(fake(map[string]string{"redoma-lead": "1964"}, map[string]string{"redoma-relay": "1963"}, nil), &log)
	for id, name := range map[uint32]string{961: "redoma-sds", 963: "redoma-relay", 964: "redoma-lead", 1964: "redoma-lead", 1963: "redoma-relay"} {
		if ids[id] != name {
			t.Errorf("id %d: %q, want %q", id, ids[id], name)
		}
	}
	if log.Len() != 0 {
		t.Errorf("logged %q for names that are simply missing", log.String())
	}

	log.Reset()
	ids = serviceIDs(fake(nil, nil, errors.New("/etc/group: permission denied")), &log)
	if !maps.Equal(ids, hook.ServiceIDs) {
		t.Errorf("a failed lookup: %v, want the static ids", ids)
	}
	if !strings.Contains(log.String(), "permission denied") {
		t.Errorf("a failed lookup was not logged: %q", log.String())
	}

	log.Reset()
	ids = serviceIDs(fake(map[string]string{"redoma-lead": "964x"}, nil, nil), &log)
	if !maps.Equal(ids, hook.ServiceIDs) || !strings.Contains(log.String(), `"964x", not a number`) {
		t.Errorf("a malformed id: %v, logged %q", ids, log.String())
	}
}
