// Package activedir is the agent.Source of a launcher, a host process that
// starts agent containers itself: one empty file per running agent in the
// active directory, named after the agent, and the agent's named network namespace,
// <netns dir>/redoma-<repo>. This package only reads that state; the
// launcher writes it. The namespace naming and the selector format of the
// launcher's agents live here and nowhere else.
package activedir

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/bpalermo/redoma/common/allowlist"
	"github.com/bpalermo/redoma/xds/internal/agent"
)

// ValidName reports whether repo can be an agent's name: the launcher's own
// check (allowlist.ValidName).
func ValidName(repo string) bool {
	return allowlist.ValidName(repo)
}

// Namespace is the path of the launcher's namespace for repo under netnsDir.
func Namespace(netnsDir, repo string) string {
	return filepath.Join(netnsDir, "redoma-"+repo)
}

// Selectors are what a launcher agent's identity is subscribed by: a type
// no attestor produces, so the identity exists only through the delegate.
// agent:<name> is the registrar-neutral selector the SPIRE entries select
// on, the same the OCI hook's agents send.
func Selectors(repo string) []agent.Selector {
	return []agent.Selector{{Type: "redoma", Value: "agent:" + repo}}
}

// Scan returns the agents that are running, sorted by ID: those with a file
// in activeDir AND a network namespace in netnsDir. An agent whose namespace
// is not there yet (the launcher is still starting it) or any more (it is
// stopping) is not running: Envoy could not bind its listeners. Entries of
// activeDir that are not agent names are returned in invalid, sorted.
//
// A missing activeDir is no agents: /run is cleared at boot, and the
// launcher creates the directory with its first agent. stat says which
// namespace is at a path (StatNetns; tests pass a fake).
func Scan(activeDir, netnsDir string, stat StatFunc) (running []agent.Agent, invalid []string, err error) {
	entries, err := os.ReadDir(activeDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		repo := e.Name()
		if !ValidName(repo) {
			invalid = append(invalid, repo)
			continue
		}
		ns := Namespace(netnsDir, repo)
		id, err := stat(ns)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		running = append(running, agent.Agent{ID: repo, Netns: ns, NetnsID: id, Selectors: Selectors(repo)})
	}
	slices.Sort(invalid)
	return running, invalid, nil
}

// StatFunc says which namespace is at path, or an error wrapping
// fs.ErrNotExist when there is none.
type StatFunc func(path string) (agent.NetnsID, error)

// StatNetns is stat(2) of the namespace file: an nsfs bind mount has the
// namespace's own device and inode.
func StatNetns(path string) (agent.NetnsID, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return agent.NetnsID{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return agent.NetnsID{}, fmt.Errorf("%s: no device and inode", path)
	}
	return agent.NetnsID{Dev: uint64(st.Dev), Ino: st.Ino}, nil
}

// Source rescans the launcher's state every Interval. Polling, not inotify:
// the state is a handful of files, a second's delay is within what the
// launcher waits for its listeners, and a rescan cannot miss an event.
type Source struct {
	ActiveDir string
	NetnsDir  string
	Interval  time.Duration
	Log       *slog.Logger
	// Stat says which namespace is at a path; nil is StatNetns.
	Stat StatFunc
}

var _ agent.Source = (*Source)(nil)

// Run implements agent.Source. A scan that fails keeps the last state: a
// transient failure must not take every agent's listeners down.
func (s *Source) Run(ctx context.Context, emit func(agent.Event)) error {
	stat := s.Stat
	if stat == nil {
		stat = StatNetns
	}
	known := map[string]agent.Agent{}
	var invalid []string
	synced := false
	tick := time.NewTicker(s.Interval)
	defer tick.Stop()
	for {
		running, bad, err := Scan(s.ActiveDir, s.NetnsDir, stat)
		if err != nil {
			s.Log.Error("reading the running agents; keeping the last state", "err", err)
		} else {
			if !slices.Equal(bad, invalid) && len(bad) > 0 {
				s.Log.Error("ignoring active entries that are not agent names", "entries", bad)
			}
			invalid = bad
			diff(known, running, emit)
			if !synced {
				synced = true
				emit(agent.Event{Synced: true})
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// diff emits what changed from known to running and updates known.
func diff(known map[string]agent.Agent, running []agent.Agent, emit func(agent.Event)) {
	now := map[string]agent.Agent{}
	for _, a := range running {
		now[a.ID] = a
	}
	for id, a := range known {
		if _, ok := now[id]; !ok {
			delete(known, id)
			emit(agent.Event{Agent: a, Removed: true})
		}
	}
	for _, a := range running {
		if old, ok := known[a.ID]; ok && old.Equal(a) {
			continue
		}
		known[a.ID] = a
		emit(agent.Event{Agent: a})
	}
}
