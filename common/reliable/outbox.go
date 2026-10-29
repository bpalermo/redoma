// Package reliable is the at-least-once delivery both ends of a relay stream
// share (api/relay/v1): the sender's queue of envelopes not yet acked, on disk
// (Outbox), the receiver's record of what it has taken (Inbox), and the loop
// that runs one stream with them (Session).
package reliable

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/protobuf/proto"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/common/atomicfile"
)

// maxRecord bounds one envelope on disk; a longer length prefix is a corrupt
// file.
const maxRecord = 16 << 20

// compactAfter is how many acked records the log may keep before it is
// rewritten with only the pending ones.
const compactAfter = 1024

// Outbox is a sender's queue: every envelope pushed and not yet acked, kept
// in a directory until the peer acks it.
//
// On disk: out.meta, {epoch, acked} replaced whole; out.log, the envelopes in
// seq order, each a 4-byte big-endian length and the marshalled Envelope. An
// envelope at or below acked is dead; the log is emptied when nothing is
// pending and rewritten when it holds compactAfter dead records. A record cut
// short by a crash is dropped when the log is opened.
type Outbox struct {
	dir  string
	sync bool

	mu      sync.Mutex
	epoch   string
	acked   uint64
	next    uint64
	pending []*relayv1.Envelope
	log     *os.File
	size    int64
	dead    int

	notify chan struct{}

	// broken is set when a failed push could not be undone.
	broken error
	// write and syncf are the log's, unless a test fails them.
	write func([]byte) error
	syncf func(*os.File) error
}

type meta struct {
	Epoch string `json:"epoch"`
	Acked uint64 `json:"acked"`
}

// OpenOutbox opens the queue in dir, made (0700) with a new epoch if it has
// none. With sync, each push is on disk (fsync) before Push returns: for a
// sender whose envelopes are few and must survive a power loss; without, a
// push survives the process but maybe not the machine.
func OpenOutbox(dir string, sync bool) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	o := &Outbox{dir: dir, sync: sync, notify: make(chan struct{}, 1), syncf: (*os.File).Sync}
	o.write = func(b []byte) error { _, err := o.log.Write(b); return err }
	m, err := o.readMeta()
	if errors.Is(err, fs.ErrNotExist) {
		m, err = meta{Epoch: NewEpoch()}, nil
		// A log without its meta is from a queue whose numbering is lost.
		if err := os.Remove(o.path("out.log")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err := o.writeMeta(m); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	o.epoch, o.acked = m.Epoch, m.Acked
	if err := o.load(); err != nil {
		return nil, err
	}
	return o, nil
}

// NewEpoch is a random epoch for a new queue.
func NewEpoch() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (o *Outbox) path(name string) string { return filepath.Join(o.dir, name) }

func (o *Outbox) readMeta() (meta, error) {
	raw, err := os.ReadFile(o.path("out.meta"))
	if err != nil {
		return meta{}, err
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return meta{}, fmt.Errorf("%s: %w", o.path("out.meta"), err)
	}
	if m.Epoch == "" {
		return meta{}, fmt.Errorf("%s: no epoch", o.path("out.meta"))
	}
	return m, nil
}

func (o *Outbox) writeMeta(m meta) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicfile.Write(o.path("out.meta"), raw, 0o600)
}

// load reads the log: the pending envelopes, and where the next seq starts.
func (o *Outbox) load() error {
	f, err := os.OpenFile(o.path("out.log"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	records, good, err := readRecords(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("%s: %w", o.path("out.log"), err)
	}
	// Whatever follows the last whole record was cut short by a crash.
	if err := f.Truncate(good); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	o.log, o.size, o.next = f, good, o.acked+1
	for _, e := range records {
		if e.GetSeq() >= o.next {
			o.next = e.GetSeq() + 1
		}
		if e.GetSeq() > o.acked {
			o.pending = append(o.pending, e)
		} else {
			o.dead++
		}
	}
	return nil
}

// readRecords reads every whole record of r, and the offset after the last.
func readRecords(r io.Reader) ([]*relayv1.Envelope, int64, error) {
	var out []*relayv1.Envelope
	var off int64
	var head [4]byte
	for {
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return out, off, nil
		}
		n := binary.BigEndian.Uint32(head[:])
		if n > maxRecord {
			return nil, 0, fmt.Errorf("a record of %d bytes at offset %d", n, off)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return out, off, nil
		}
		e := relayv1.Envelope_builder{}.Build()
		if err := proto.Unmarshal(buf, e); err != nil {
			return nil, 0, fmt.Errorf("the record at offset %d: %w", off, err)
		}
		out = append(out, e)
		off += int64(len(head)) + int64(n)
	}
}

// record is e as the log holds it: its length, then e.
func record(e *relayv1.Envelope) ([]byte, error) {
	b, err := proto.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(b) > maxRecord {
		return nil, fmt.Errorf("an envelope of %d bytes is over %d", len(b), maxRecord)
	}
	return append(binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(b)), uint32(len(b))), b...), nil
}

// Epoch is the queue's epoch, sent to the peer as "redoma-epoch".
func (o *Outbox) Epoch() string { return o.epoch }

// Push gives e the next seq and queues it on disk. e is the queue's from
// then on: the caller does not change it.
func (o *Outbox) Push(e *relayv1.Envelope) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.broken != nil {
		return o.broken
	}
	e.SetSeq(o.next)
	rec, err := record(e)
	if err != nil {
		e.SetSeq(0)
		return err
	}
	err = o.write(rec)
	if err == nil && o.sync {
		err = o.syncf(o.log)
	}
	if err != nil {
		e.SetSeq(0)
		// Leave no part of it for the next open to misread, nor a record
		// whose seq the next push would reuse. If even that fails, the
		// queue refuses every push from then on: its log is no longer
		// known good.
		if terr := o.undo(); terr != nil {
			o.broken = fmt.Errorf("the queue's log could not be cut back after a failed push (%v): %w", err, terr)
		}
		return err
	}
	o.size += int64(len(rec))
	o.next++
	o.pending = append(o.pending, e)
	select {
	case o.notify <- struct{}{}:
	default:
	}
	return nil
}

// undo cuts the log back to its last good record.
func (o *Outbox) undo() error {
	if err := o.log.Truncate(o.size); err != nil {
		return err
	}
	_, err := o.log.Seek(o.size, io.SeekStart)
	return err
}

// Ack drops every envelope up to seq: the peer has taken them. A seq beyond
// the last pushed acks everything pushed.
func (o *Outbox) Ack(seq uint64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	seq = min(seq, o.next-1)
	if seq <= o.acked {
		return nil
	}
	if err := o.writeMeta(meta{Epoch: o.epoch, Acked: seq}); err != nil {
		return err
	}
	o.acked = seq
	n := 0
	for n < len(o.pending) && o.pending[n].GetSeq() <= seq {
		n++
	}
	o.pending = o.pending[n:]
	o.dead += n
	switch {
	case len(o.pending) == 0:
		return o.rewrite()
	case o.dead >= compactAfter:
		return o.rewrite()
	}
	return nil
}

// rewrite replaces the log with the pending envelopes alone.
func (o *Outbox) rewrite() error {
	var buf []byte
	for _, e := range o.pending {
		rec, err := record(e)
		if err != nil {
			return err
		}
		buf = append(buf, rec...)
	}
	if err := atomicfile.Write(o.path("out.log"), buf, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(o.path("out.log"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return err
	}
	o.log.Close()
	o.log, o.size, o.dead = f, int64(len(buf)), 0
	return nil
}

// After returns the pending envelopes whose seq is above seq, in order.
func (o *Outbox) After(seq uint64) []*relayv1.Envelope {
	o.mu.Lock()
	defer o.mu.Unlock()
	i := 0
	for i < len(o.pending) && o.pending[i].GetSeq() <= seq {
		i++
	}
	return append([]*relayv1.Envelope(nil), o.pending[i:]...)
}

// Len is how many envelopes are pending.
func (o *Outbox) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

// Pushed is signalled after a Push; one Session at a time waits on it.
func (o *Outbox) Pushed() <-chan struct{} { return o.notify }

// Close closes the log.
func (o *Outbox) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.log.Close()
}
