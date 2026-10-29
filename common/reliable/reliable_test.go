package reliable

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
)

func task(id string) *relayv1.Envelope {
	return relayv1.Envelope_builder{Task: relayv1.Task_builder{Id: id, Text: "do " + id}.Build()}.Build()
}

func ids(es []*relayv1.Envelope) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.GetTask().GetId())
	}
	return out
}

func seqs(es []*relayv1.Envelope) []uint64 {
	var out []uint64
	for _, e := range es {
		out = append(out, e.GetSeq())
	}
	return out
}

func mustOutbox(t *testing.T, dir string) *Outbox {
	t.Helper()
	o, err := OpenOutbox(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

func push(t *testing.T, o *Outbox, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := o.Push(task(n)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOutboxSurvivesReopen: what is pushed and not acked is there again,
// with its seq, after the queue is opened again (a restart), and the
// numbering and the epoch carry on.
func TestOutboxSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	o := mustOutbox(t, dir)
	push(t, o, "a", "b", "c")
	if err := o.Ack(1); err != nil {
		t.Fatal(err)
	}
	epoch := o.Epoch()
	o.Close()

	o = mustOutbox(t, dir)
	if o.Epoch() != epoch {
		t.Errorf("epoch %q after reopen, want %q", o.Epoch(), epoch)
	}
	if got := o.After(0); !slices.Equal(ids(got), []string{"b", "c"}) || !slices.Equal(seqs(got), []uint64{2, 3}) {
		t.Errorf("pending after reopen: %v %v, want [b c] [2 3]", ids(got), seqs(got))
	}
	push(t, o, "d")
	if got := seqs(o.After(3)); !slices.Equal(got, []uint64{4}) {
		t.Errorf("next seq after reopen: %v, want [4]", got)
	}
}

// TestOutboxEmptyKeepsNumbering: once everything is acked the log is
// emptied, but the next push still follows the last seq.
func TestOutboxEmptyKeepsNumbering(t *testing.T) {
	dir := t.TempDir()
	o := mustOutbox(t, dir)
	push(t, o, "a", "b")
	if err := o.Ack(2); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "out.log")); err != nil || fi.Size() != 0 {
		t.Errorf("out.log after acking everything: %v, %v; want empty", fi, err)
	}
	o.Close()
	o = mustOutbox(t, dir)
	push(t, o, "c")
	if got := seqs(o.After(0)); !slices.Equal(got, []uint64{3}) {
		t.Errorf("seq after an emptied log: %v, want [3]", got)
	}
}

// TestOutboxDropsATornRecord: a record cut short (a crash in the middle of
// a write) is dropped, and the records before it are kept.
func TestOutboxDropsATornRecord(t *testing.T) {
	dir := t.TempDir()
	o := mustOutbox(t, dir)
	push(t, o, "a", "b")
	o.Close()
	path := filepath.Join(dir, "out.log")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-3], 0o600); err != nil {
		t.Fatal(err)
	}
	o = mustOutbox(t, dir)
	if got := ids(o.After(0)); !slices.Equal(got, []string{"a"}) {
		t.Errorf("pending after a torn record: %v, want [a]", got)
	}
	push(t, o, "c")
	o.Close()
	o = mustOutbox(t, dir)
	if got := seqs(o.After(0)); !slices.Equal(got, []uint64{1, 2}) {
		t.Errorf("after a torn record and a push: %v, want [1 2]", got)
	}
}

// TestOutboxCompacts: the log does not grow without end while the peer acks
// but something stays pending.
func TestOutboxCompacts(t *testing.T) {
	dir := t.TempDir()
	o := mustOutbox(t, dir)
	const n = compactAfter + 10
	for range n {
		push(t, o, "x")
	}
	if err := o.Ack(n - 2); err != nil {
		t.Fatal(err)
	}
	if o.dead != 0 {
		t.Errorf("%d dead records after an ack past compactAfter, want 0", o.dead)
	}
	o.Close()
	o = mustOutbox(t, dir)
	if o.dead != 0 {
		t.Errorf("%d dead records in the log after compaction, want 0", o.dead)
	}
	if got := seqs(o.After(0)); !slices.Equal(got, []uint64{n - 1, n}) {
		t.Errorf("pending: %v, want [%d %d]", got, n-1, n)
	}
}

// TestOutboxNewWithoutMeta: a log whose meta is gone starts a new queue
// with a new epoch, since its numbering is lost.
func TestOutboxNewWithoutMeta(t *testing.T) {
	dir := t.TempDir()
	o := mustOutbox(t, dir)
	push(t, o, "a")
	epoch := o.Epoch()
	o.Close()
	if err := os.Remove(filepath.Join(dir, "out.meta")); err != nil {
		t.Fatal(err)
	}
	o = mustOutbox(t, dir)
	if o.Epoch() == epoch || o.Len() != 0 {
		t.Errorf("epoch %q (was %q), %d pending; want a new, empty queue", o.Epoch(), epoch, o.Len())
	}
}

// TestInbox: a seq is taken once; a new epoch starts again; only what was
// saved is acked, and it survives a reopen.
func TestInbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.json")
	in, err := OpenInbox(path)
	if err != nil {
		t.Fatal(err)
	}
	var took []uint64
	take := func(i *Inbox, epoch string, seq uint64) {
		i.Take(epoch, seq, func() { took = append(took, seq) })
	}
	if _, ok, _ := in.Flush(); ok {
		t.Error("an ack before anything was taken")
	}
	take(in, "e1", 1)
	take(in, "e1", 2)
	take(in, "e1", 2)
	take(in, "e1", 1)
	if seq, ok, err := in.Flush(); err != nil || !ok || seq != 2 {
		t.Errorf("Flush = %d, %v, %v; want 2", seq, ok, err)
	}
	if _, ok, _ := in.Flush(); ok {
		t.Error("an ack with nothing new")
	}
	take(in, "e1", 2)
	if seq, ok, _ := in.Flush(); !ok || seq != 2 {
		t.Errorf("a duplicate is acked again: %d, %v", seq, ok)
	}

	in, err = OpenInbox(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Saved("e1"); got != 2 {
		t.Errorf("Saved after reopen = %d, want 2", got)
	}
	take(in, "e1", 2)
	take(in, "e1", 3)
	take(in, "e2", 1)
	if want := []uint64{1, 2, 3, 1}; !slices.Equal(took, want) {
		t.Errorf("took %v, want %v", took, want)
	}
	if got := in.Saved("e1"); got != 2 {
		t.Errorf("Saved of the old epoch after a new one = %d (not flushed yet: still e1's)", got)
	}
}

// pipe is an in-memory Connect stream between two Sessions; break_ fails
// both ends, as a dropped connection does.
type pipe struct {
	ab, ba chan *relayv1.Envelope
	done   chan struct{}
	once   sync.Once
}

type end struct {
	in, out chan *relayv1.Envelope
	p       *pipe
}

func newPipe() (*pipe, *end, *end) {
	p := &pipe{ab: make(chan *relayv1.Envelope, 64), ba: make(chan *relayv1.Envelope, 64), done: make(chan struct{})}
	return p, &end{in: p.ba, out: p.ab, p: p}, &end{in: p.ab, out: p.ba, p: p}
}

var errBroken = errors.New("the connection broke")

func (p *pipe) break_() { p.once.Do(func() { close(p.done) }) }

func (e *end) Send(m *relayv1.Envelope) error {
	select {
	case e.out <- m:
		return nil
	case <-e.p.done:
		return errBroken
	}
}

func (e *end) Recv() (*relayv1.Envelope, error) {
	select {
	case m := <-e.in:
		return m, nil
	case <-e.p.done:
		return nil, errBroken
	}
}

type peer struct {
	out *Outbox
	in  *Inbox

	mu  sync.Mutex
	got []string
}

func newPeer(t *testing.T, dir string) *peer {
	t.Helper()
	o := mustOutbox(t, filepath.Join(dir, "out"))
	in, err := OpenInbox(filepath.Join(dir, "in.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &peer{out: o, in: in}
}

func (p *peer) session(other *peer) *Session {
	return &Session{
		Out: p.out, In: p.in, PeerEpoch: other.out.Epoch(), AckEvery: time.Millisecond,
		Handle: func(e *relayv1.Envelope) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.got = append(p.got, e.GetTask().GetId())
		},
	}
}

func (p *peer) received() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

// connect runs a and b over a new pipe until it is broken.
func connect(a, b *peer) (*pipe, func()) {
	p, ea, eb := newPipe()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = a.session(b).Run(context.Background(), ea) }()
	go func() { defer wg.Done(); _ = b.session(a).Run(context.Background(), eb) }()
	return p, func() { p.break_(); wg.Wait() }
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSessionResendsAcrossADisconnect: what was pushed while the stream was
// down, and what was sent but not acked when it broke, arrives once the
// peers connect again, each envelope handled exactly once and acked.
func TestSessionResendsAcrossADisconnect(t *testing.T) {
	dir := t.TempDir()
	a, b := newPeer(t, filepath.Join(dir, "a")), newPeer(t, filepath.Join(dir, "b"))

	_, stop := connect(a, b)
	push(t, a.out, "1", "2")
	push(t, b.out, "x")
	eventually(t, "the first envelopes", func() bool { return len(b.received()) == 2 && len(a.received()) == 1 })
	eventually(t, "the acks", func() bool { return a.out.Len() == 0 && b.out.Len() == 0 })
	stop()

	// Pushed while nothing is connected.
	push(t, a.out, "3", "4")
	// Sent, but the stream breaks before the ack: a's ack ticker is not
	// given the time.
	p, ea, eb := newPipe()
	bs := b.session(a)
	bs.AckEvery = time.Hour
	done := make(chan struct{}, 2)
	go func() { _ = a.session(b).Run(context.Background(), ea); done <- struct{}{} }()
	go func() { _ = bs.Run(context.Background(), eb); done <- struct{}{} }()
	eventually(t, "3 and 4 handled", func() bool { return len(b.received()) == 4 })
	p.break_()
	<-done
	<-done
	if a.out.Len() != 2 {
		t.Fatalf("a has %d pending, want 2 (3 and 4, never acked)", a.out.Len())
	}

	_, stop = connect(a, b)
	push(t, a.out, "5")
	eventually(t, "5", func() bool { return len(b.received()) == 5 })
	eventually(t, "everything acked", func() bool { return a.out.Len() == 0 })
	stop()
	if got, want := b.received(), []string{"1", "2", "3", "4", "5"}; !slices.Equal(got, want) {
		t.Errorf("b handled %v, want %v once each", got, want)
	}
	if got, want := a.received(), []string{"x"}; !slices.Equal(got, want) {
		t.Errorf("a handled %v, want %v", got, want)
	}
}

// TestSessionAfterReceiverRestart: a receiver opened again from its files
// takes nothing twice that it had acked, and a sender opened again resends
// what was pending.
func TestSessionAfterReceiverRestart(t *testing.T) {
	dir := t.TempDir()
	a, b := newPeer(t, filepath.Join(dir, "a")), newPeer(t, filepath.Join(dir, "b"))
	_, stop := connect(a, b)
	push(t, a.out, "1", "2")
	eventually(t, "the acks", func() bool { return a.out.Len() == 0 })
	stop()
	push(t, a.out, "3")
	a.out.Close()
	b.out.Close()

	a2, b2 := newPeer(t, filepath.Join(dir, "a")), newPeer(t, filepath.Join(dir, "b"))
	_, stop = connect(a2, b2)
	eventually(t, "3 after the restart", func() bool { return len(b2.received()) == 1 })
	eventually(t, "the ack", func() bool { return a2.out.Len() == 0 })
	stop()
	if got := b2.received(); !slices.Equal(got, []string{"3"}) {
		t.Errorf("after a restart b handled %v, want [3]", got)
	}
}

// TestSessionNewPeerEpoch: a peer whose queue was made again from nothing
// (its seqs start at 1 again) is taken from its first envelope, not refused
// as old.
func TestSessionNewPeerEpoch(t *testing.T) {
	dir := t.TempDir()
	a, b := newPeer(t, filepath.Join(dir, "a")), newPeer(t, filepath.Join(dir, "b"))
	_, stop := connect(a, b)
	push(t, a.out, "1", "2")
	eventually(t, "the acks", func() bool { return a.out.Len() == 0 })
	stop()
	a.out.Close()
	if err := os.RemoveAll(filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	a2 := newPeer(t, filepath.Join(dir, "a"))
	_, stop = connect(a2, b)
	push(t, a2.out, "new")
	eventually(t, "the new queue's first envelope", func() bool { return len(b.received()) == 3 })
	stop()
	if got := b.received(); got[2] != "new" {
		t.Errorf("b handled %v, want the new queue's envelope last", got)
	}
}

// TestSessionRefusesNoSeq: an envelope that should carry a seq and does not
// ends the stream.
func TestSessionRefusesNoSeq(t *testing.T) {
	a := newPeer(t, t.TempDir())
	_, ea, eb := newPipe()
	errc := make(chan error, 1)
	go func() { errc <- a.session(a).Run(context.Background(), ea) }()
	if err := eb.Send(task("noseq")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Error("Run returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end")
	}
}

// TestOutboxFailedSync: a push whose sync fails leaves nothing behind: its
// record is cut from the log, its seq is the next push's, and a reopen
// finds only the good records. A push whose cut fails too breaks the queue
// for good rather than risk a second record of one seq.
func TestOutboxFailedSync(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutbox(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	push(t, o, "a")
	o.syncf = func(*os.File) error { return errors.New("disk on fire") }
	if err := o.Push(task("lost")); err == nil {
		t.Fatal("a push whose sync failed succeeded")
	}
	o.syncf = (*os.File).Sync
	push(t, o, "b")
	if got := seqs(o.After(0)); !slices.Equal(got, []uint64{1, 2}) {
		t.Errorf("after a failed sync: seqs %v, want [1 2]", got)
	}
	o.Close()
	o = mustOutbox(t, dir)
	if got, gotIDs := seqs(o.After(0)), ids(o.After(0)); !slices.Equal(got, []uint64{1, 2}) || !slices.Equal(gotIDs, []string{"a", "b"}) {
		t.Errorf("reopened: %v %v, want [1 2] [a b]", got, gotIDs)
	}

	o.write = func([]byte) error { return errors.New("short write") }
	o.log.Close() // the cut fails on a closed file
	if err := o.Push(task("x")); err == nil {
		t.Fatal("a failed write succeeded")
	}
	o.write = func([]byte) error { return nil }
	if err := o.Push(task("y")); err == nil || !strings.Contains(err.Error(), "could not be cut back") {
		t.Errorf("a push after a failed cut: %v, want the queue broken", err)
	}
}

// TestFlushAfterAcksOnlyWhatBeforeCovered: an envelope taken while the
// pre-ack sync runs is not in that ack: its handler's writes may not be
// durable yet. It is in the next one.
func TestFlushAfterAcksOnlyWhatBeforeCovered(t *testing.T) {
	in, err := OpenInbox(filepath.Join(t.TempDir(), "in.json"))
	if err != nil {
		t.Fatal(err)
	}
	in.Take("e", 1, func() {})
	seq, ok, err := in.FlushAfter(func() error {
		in.Take("e", 2, func() {})
		return nil
	})
	if err != nil || !ok || seq != 1 {
		t.Errorf("FlushAfter = %d, %v, %v; want 1", seq, ok, err)
	}
	if got := in.Saved("e"); got != 1 {
		t.Errorf("saved %d, want 1", got)
	}
	if seq, ok, _ := in.Flush(); !ok || seq != 2 {
		t.Errorf("the next Flush = %d, %v; want 2", seq, ok)
	}
	in.Take("e", 3, func() {})
	if _, ok, err := in.FlushAfter(func() error { return errors.New("sync failed") }); ok || err != nil {
		t.Errorf("a failed before: ack %v, %v; want none, no error", ok, err)
	}
	if got := in.Saved("e"); got != 2 {
		t.Errorf("saved %d after a failed before, want 2", got)
	}
}
