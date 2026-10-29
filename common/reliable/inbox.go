package reliable

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/bpalermo/redoma/common/atomicfile"
)

// Inbox is a receiver's record of what it has taken from its peer: the
// peer's epoch and the highest seq taken, so that what the peer resends is
// taken once. Taken is what the handler has finished with; only that is
// saved, and only what is saved is acked, so an envelope acked is one that
// was handled.
type Inbox struct {
	path string

	mu    sync.Mutex
	cur   inState
	saved inState
	// reack is set when a duplicate arrived: the peer lacks an ack for it.
	reack bool
}

type inState struct {
	PeerEpoch string `json:"peer_epoch"`
	Last      uint64 `json:"last"`
}

// OpenInbox opens the record at path (a JSON file, replaced whole), empty if
// there is none.
func OpenInbox(path string) (*Inbox, error) {
	i := &Inbox{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return i, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &i.cur); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	i.saved = i.cur
	return i, nil
}

// Take runs handle if seq, from a peer queue of epoch, is new: above the
// last taken, or from another epoch than the last, which starts the count
// again. Takes are serialized, so two streams of the same peer (one
// replacing the other) never handle the same seq at once.
func (i *Inbox) Take(epoch string, seq uint64, handle func()) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if epoch != i.cur.PeerEpoch {
		i.cur = inState{PeerEpoch: epoch}
	}
	if seq <= i.cur.Last {
		i.reack = true
		return false
	}
	handle()
	i.cur.Last = seq
	return true
}

// Saved is the last seq saved from the peer queue of epoch, 0 for none or
// for another epoch.
func (i *Inbox) Saved(epoch string) uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.saved.PeerEpoch != epoch {
		return 0
	}
	return i.saved.Last
}

// Flush saves what was taken since the last flush and returns the seq to ack
// for it, or false when the peer needs no ack.
func (i *Inbox) Flush() (uint64, bool, error) { return i.FlushAfter(nil) }

// FlushAfter is Flush, with before run first: it makes durable what the
// handler wrote for the envelopes taken so far (Session.BeforeAck). Only
// those are saved and acked; one taken while before runs waits for the next
// flush, since what its handler wrote may not be durable yet. A failing
// before skips this flush.
func (i *Inbox) FlushAfter(before func() error) (uint64, bool, error) {
	i.mu.Lock()
	snap := i.cur
	i.mu.Unlock()
	if before != nil {
		if err := before(); err != nil {
			return 0, false, nil
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if snap != i.saved {
		raw, err := json.Marshal(snap)
		if err != nil {
			return 0, false, err
		}
		if err := atomicfile.Write(i.path, raw, 0o600); err != nil {
			return 0, false, err
		}
		i.saved, i.reack = snap, true
	}
	if !i.reack || i.saved.Last == 0 {
		return 0, false, nil
	}
	i.reack = false
	return i.saved.Last, true, nil
}
