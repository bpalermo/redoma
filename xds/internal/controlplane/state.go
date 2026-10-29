package controlplane

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/bpalermo/redoma/common/atomicfile"
	"github.com/bpalermo/redoma/xds/internal/agent"
)

// Which namespace Envoy may hold listeners in, across restarts.
//
// Envoy keeps its listeners while this process is down. A namespace replaced
// at an agent's path in the meantime would otherwise get the same listeners,
// byte for byte, from the new process, and Envoy would keep the old socket.
// So every path whose listeners Envoy may hold (a running agent's, or a
// removal not yet cleared) is recorded with its namespace in StateFile,
// written before each snapshot that could bind something new. At start each
// recorded path is a removal in the first snapshot: an agent found in the
// recorded namespace is bound in it at once, one in another namespace waits
// for Envoy's report like any replacement.
//
// The file lives in /run/redoma-xds, a tmpfs: a reboot, which also restarts
// Envoy and clears every namespace, clears it too. A file that cannot be
// read is logged and ignored; agents replaced while this process was down
// then keep their old listeners until they are restarted.

// stateFile is StateFile's content.
type stateFile struct {
	// Held maps a namespace path to the namespace there.
	Held map[string]agent.NetnsID `json:"held"`
}

// held is what Envoy may hold now.
func (c *Controller) held() map[string]agent.NetnsID {
	out := map[string]agent.NetnsID{}
	for path, r := range c.removedIn {
		out[path] = r.held
	}
	for _, a := range c.agents {
		out[a.agent.Netns] = a.agent.NetnsID
	}
	return out
}

// load turns the previous process's record into removals in the first
// snapshot, which is version 1.
func (c *Controller) load() {
	if c.cfg.StateFile == "" {
		return
	}
	// Temporary files of a process that died between writing and renaming.
	if stale, err := filepath.Glob(c.cfg.StateFile + ".*"); err == nil {
		for _, f := range stale {
			os.Remove(f)
		}
	}
	raw, err := os.ReadFile(c.cfg.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var st stateFile
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	if err != nil {
		c.log.Error("cannot read which namespaces Envoy holds; an agent whose namespace was replaced while redoma-xds was down keeps its old listeners until it is restarted", "file", c.cfg.StateFile, "err", err)
		return
	}
	now := time.Now()
	for path, id := range st.Held {
		c.removedIn[path] = &removal{version: c.version + 1, since: now, held: id}
	}
	c.saved = st.Held
	c.loaded = len(st.Held)
}

// save writes the record if it changed, atomically. A failure is logged:
// it only costs the protection across a restart.
func (c *Controller) save() {
	if c.cfg.StateFile == "" {
		return
	}
	held := c.held()
	if c.saved != nil && maps.Equal(held, c.saved) {
		return
	}
	raw, err := json.Marshal(stateFile{Held: held})
	if err == nil {
		err = atomicfile.Write(c.cfg.StateFile, raw, 0o600)
	}
	if err != nil {
		c.log.Error("cannot record which namespaces Envoy holds", "file", c.cfg.StateFile, "err", err)
		return
	}
	c.saved = held
}
