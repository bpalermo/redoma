package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/bpalermo/redoma/common/atomicfile"
)

// store is an agent's state that the lead depends on, in a journal
// (lead.log, one JSON record per line, appended): the tasks the relay gave
// it that are still open, its pending questions, and its messages not yet
// delivered to a watch. Every size and count in it is bounded (limits.go),
// so the journal is compacted, rewritten from the state, now and then
// rather than on every change; an append is not synced, the agent's acks
// are (syncs before the ack, Session.BeforeAck). A torn last line, from a
// crash in the middle of an append, is dropped when it is read; a bad line
// anywhere else refuses the journal.
type store struct {
	path    string
	f       *os.File
	records int
	dirty   bool
	size    int64
	// broken is why the journal can no longer be trusted (a failed write
	// that could not be cut back, a file renamed but not reopened): every
	// later add and sync fails with it. compactErr is the last compaction
	// that failed without breaking anything, for the log.
	broken     error
	compactErr error
	// write, truncate, reopen and replace are the file's (replace is
	// atomicfile.Write), unless a test fails them.
	write    func([]byte) (int, error)
	truncate func(int64) error
	reopen   func() (*os.File, error)
	replace  func(path string, data []byte, mode os.FileMode) error

	tasks []string
	// cancelled are tasks the lead cancelled whose last event has not come:
	// they take turn output (the cancelled turn's end) but no notes, and
	// leave the tasks' count, so an agent cannot hold the lead's quota by
	// never ending them; the oldest beyond maxOpenTasks are forgotten.
	cancelled []string
	questions []held
	messages  []held
	msgBytes  int
	nextMsg   uint64
}

// held is a question (ID) or a message (N) the lead has not seen.
type held struct {
	ID     string `json:"id,omitempty"`
	N      uint64 `json:"n,omitempty"`
	Text   string `json:"text,omitempty"`
	TaskID string `json:"task,omitempty"`
}

// record is one line of the journal.
type record struct {
	// Op is task (opened), cancel, close, question, answered, message or delivered.
	Op string `json:"op"`
	held
}

func openStore(path string) (*store, error) {
	s := &store{path: path, nextMsg: 1}
	s.write = func(b []byte) (int, error) { return s.f.Write(b) }
	s.truncate = func(n int64) error { return s.f.Truncate(n) }
	s.reopen = func() (*os.File, error) { return os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0) }
	s.replace = atomicfile.Write
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	lines := bytes.Split(raw, []byte("\n"))
	for i, line := range lines {
		if len(line) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			// Only the last line, with no newline after it, can be an
			// append cut short; anything else is a journal not to trust.
			if i == len(lines)-1 {
				break
			}
			return nil, fmt.Errorf("%s: line %d is corrupt (%v); refusing the agent's journal rather than lose what follows", path, i+1, err)
		}
		s.apply(r)
		s.records++
	}
	// Rewritten at once: a torn line goes, and appends start clean.
	if err := s.compact(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *store) apply(r record) {
	switch r.Op {
	case "task":
		if !slices.Contains(s.tasks, r.ID) {
			s.tasks = append(s.tasks, r.ID)
		}
	case "cancel":
		if slices.Contains(s.tasks, r.ID) {
			s.tasks = slices.DeleteFunc(s.tasks, func(t string) bool { return t == r.ID })
			s.cancelled = append(s.cancelled, r.ID)
			if n := len(s.cancelled) - maxOpenTasks; n > 0 {
				s.cancelled = s.cancelled[n:]
			}
		}
	case "close":
		s.tasks = slices.DeleteFunc(s.tasks, func(t string) bool { return t == r.ID })
		s.cancelled = slices.DeleteFunc(s.cancelled, func(t string) bool { return t == r.ID })
	case "question":
		s.questions = append(s.questions, r.held)
	case "answered":
		s.questions = slices.DeleteFunc(s.questions, func(q held) bool { return q.ID == r.ID })
	case "message":
		s.messages = append(s.messages, r.held)
		s.msgBytes += heldSize(r.Text)
		s.nextMsg = max(s.nextMsg, r.N+1)
	case "delivered":
		s.messages = slices.DeleteFunc(s.messages, func(m held) bool {
			if m.N == r.N {
				s.msgBytes -= heldSize(m.Text)
				return true
			}
			return false
		})
	}
}

// add appends r to the journal and then applies it: a record that could not
// be written is not in the state either, and is cut from the journal.
func (s *store) add(r record) error {
	if s.broken != nil {
		return s.broken
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := s.write(b); err != nil {
		if terr := s.truncate(s.size); terr != nil {
			// Torn bytes may stay at the end, and the next append would
			// land after them: no more appends.
			s.broken = fmt.Errorf("the journal could not be cut back after a failed write (%v): %w", err, terr)
			return s.broken
		}
		return err
	}
	s.size += int64(len(b))
	s.apply(r)
	s.records++
	s.dirty = true
	if s.records > 4*(len(s.tasks)+len(s.cancelled)+len(s.questions)+len(s.messages))+256 {
		// The record is written and applied: a compaction that fails does
		// not undo it (a broken store says so from the next add on).
		s.compactErr = s.compact()
	}
	return nil
}

// compact rewrites the journal from the state.
func (s *store) compact() error {
	var buf []byte
	n := 0
	put := func(r record) {
		b, _ := json.Marshal(r)
		buf = append(append(buf, b...), '\n')
		n++
	}
	for _, t := range s.tasks {
		put(record{Op: "task", held: held{ID: t}})
	}
	for _, t := range s.cancelled {
		put(record{Op: "task", held: held{ID: t}})
		put(record{Op: "cancel", held: held{ID: t}})
	}
	for _, q := range s.questions {
		put(record{Op: "question", held: q})
	}
	for _, m := range s.messages {
		put(record{Op: "message", held: m})
	}
	if err := s.replace(s.path, buf, 0o600); err != nil {
		// atomicfile.Write can fail after its rename (syncing the
		// directory): the path is then the new journal, and appends to
		// the open, old one would be lost. Otherwise the old journal is
		// untouched, and appends go on to it.
		if s.f == nil || s.renamed() {
			if f, rerr := s.reopen(); rerr == nil {
				s.swap(f, n, len(buf))
				return err
			}
			s.broken = fmt.Errorf("the compacted journal could not be opened after a failed compaction (%v)", err)
			return s.broken
		}
		return err
	}
	f, err := s.reopen()
	if err != nil {
		if s.f != nil {
			// The open file is the old, unlinked journal: what would be
			// appended to it is lost on the next open.
			s.broken = fmt.Errorf("the compacted journal could not be opened: %w", err)
			return s.broken
		}
		return err
	}
	s.swap(f, n, len(buf))
	return nil
}

// swap makes f, holding n records of size bytes, the journal appended to.
func (s *store) swap(f *os.File, n, size int) {
	if s.f != nil {
		s.f.Close()
	}
	s.f, s.records, s.dirty, s.size = f, n, false, int64(size)
}

// renamed says that the path is no longer the file appended to.
func (s *store) renamed() bool {
	open, err := s.f.Stat()
	if err != nil {
		return true
	}
	now, err := os.Stat(s.path)
	return err != nil || !os.SameFile(open, now)
}

// sync makes the appends since the last sync durable.
func (s *store) sync() error {
	if s.broken != nil {
		return s.broken
	}
	if !s.dirty {
		return nil
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func (s *store) close() error { return s.f.Close() }

// open says task is open; or, with cancelled, open or cancelled and not
// yet ended.
func (s *store) open(task string, cancelled bool) bool {
	return slices.Contains(s.tasks, task) || (cancelled && slices.Contains(s.cancelled, task))
}

func (s *store) question(id string) (held, bool) {
	i := slices.IndexFunc(s.questions, func(q held) bool { return q.ID == id })
	if i < 0 {
		return held{}, false
	}
	return s.questions[i], true
}
