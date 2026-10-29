package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/common/atomicfile"
)

func openTestStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "lead.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

// TestStoreFailedWriteCutBack: a write that fails on an open file is cut
// back, and the store goes on.
func TestStoreFailedWriteCutBack(t *testing.T) {
	s := openTestStore(t)
	write := s.write
	s.write = func(b []byte) (int, error) {
		n, _ := write(b[:len(b)/2]) // torn
		return n, errors.New("disk full")
	}
	if err := s.add(record{Op: "task", held: held{ID: "a/1"}}); err == nil {
		t.Fatal("a failed write succeeded")
	}
	if s.broken != nil || s.open("a/1", true) {
		t.Errorf("broken %v, task applied %v: want neither", s.broken, s.open("a/1", true))
	}
	s.write = write
	if err := s.add(record{Op: "task", held: held{ID: "a/2"}}); err != nil {
		t.Fatal(err)
	}
	s.close()
	again, err := openStore(s.path)
	if err != nil {
		t.Fatalf("the journal after a cut-back write: %v", err)
	}
	defer again.close()
	if !again.open("a/2", false) || again.open("a/1", false) {
		t.Errorf("tasks %v, want [a/2]", again.tasks)
	}
}

// TestStoreBrokenAfterFailedCutBack: when the cut back fails too, the store
// is broken: nothing more is appended or synced.
func TestStoreBrokenAfterFailedCutBack(t *testing.T) {
	s := openTestStore(t)
	s.write = func([]byte) (int, error) { return 0, errors.New("disk full") }
	s.truncate = func(int64) error { return errors.New("read-only now") }
	if err := s.add(record{Op: "task", held: held{ID: "a/1"}}); err == nil || s.broken == nil {
		t.Fatalf("add = %v, broken %v; want broken", err, s.broken)
	}
	s.write = func(b []byte) (int, error) { return len(b), nil }
	if err := s.add(record{Op: "task", held: held{ID: "a/2"}}); err == nil || s.open("a/2", true) {
		t.Error("a broken store took an append")
	}
	if err := s.sync(); err == nil {
		t.Error("a broken store synced")
	}
}

// TestStoreCompactionFailure: a compaction that fails does not fail the
// append it follows, which stays written and applied; a compaction whose
// new file cannot be reopened breaks the store.
func TestStoreCompactionFailure(t *testing.T) {
	s := openTestStore(t)
	dir := filepath.Dir(s.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		f.Close()
		os.Remove(f.Name())
		t.Skip("the directory is still writable (root?)")
	}
	for i := 0; i < 300; i++ {
		if err := s.add(record{Op: "message", held: held{N: s.nextMsg, Text: "x"}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if err := s.add(record{Op: "delivered", held: held{N: s.nextMsg - 1}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if s.compactErr == nil || s.broken != nil {
		t.Errorf("compactErr %v, broken %v; want a failed compaction, not a broken store", s.compactErr, s.broken)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	s.compactErr = nil
	s.reopen = func() (*os.File, error) { return nil, errors.New("too many open files") }
	for s.broken == nil {
		if err := s.add(record{Op: "message", held: held{N: s.nextMsg, Text: "x"}}); err != nil {
			t.Fatalf("the append whose compaction breaks the store failed: %v", err)
		}
		if s.records > 10000 {
			t.Fatal("no compaction")
		}
	}
	if err := s.add(record{Op: "task", held: held{ID: "a/1"}}); err == nil {
		t.Error("a store whose new journal could not be opened took an append")
	}
}

// TestQuarantine: one agent's corrupt journal quarantines that agent only:
// the relay starts, serves the others, refuses it, and tells the lead.
func TestQuarantine(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	b := e.mustConnect(r, "alpha", "shim-b")
	give(t, r, b, "alpha", "go")
	a := e.mustConnect(r, "beta", "shim-a")
	give(t, r, a, "beta", "go")
	r.stop()
	journal := filepath.Join(e.dir, "agents", "alpha", "lead.log")
	if err := os.WriteFile(journal, []byte("{\"op\":\"task\",\"id\":\"alpha/1\"}\nnot json\n{\"op\":\"task\",\"id\":\"alpha/2\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r = e.start()
	events, _ := watch(t, r, "")
	ev := next(t, events)
	if ev.GetAgent() != "alpha" || !strings.Contains(ev.GetDropped().GetReason(), "quarantined") && !strings.Contains(ev.GetDropped().GetReason(), "broken") {
		t.Errorf("the watch's first event: %v, want alpha's quarantine", ev)
	}
	if _, err := r.lead.Send(context.Background(), &relayv1.SendRequest{Agent: "alpha", Text: "x"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a task for the quarantined agent: %v, want FailedPrecondition", err)
	}
	if b, err := e.connect(r, "spiffe://"+td+"/agent/alpha", "shim-b"); err == nil {
		if err := b.endErr(); status.Code(err) != codes.Unavailable {
			t.Errorf("the quarantined agent's stream: %v, want Unavailable", err)
		}
	}
	// The other agent is served.
	a = e.mustConnect(r, "beta", "shim-a")
	if got := a.recv(); got.GetTask().GetText() != "go" {
		t.Errorf("the unacked task again: %v", got)
	}
	give(t, r, a, "beta", "still served")

	// Reply and Cancel are refused as Send is, not "not found".
	if _, err := r.lead.Reply(context.Background(), &relayv1.ReplyRequest{QuestionId: "alpha/q1", Text: "x"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a reply for the quarantined agent: %v, want FailedPrecondition", err)
	}
	if _, err := r.lead.Cancel(context.Background(), &relayv1.CancelRequest{TaskId: "alpha/1"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a cancel for the quarantined agent: %v, want FailedPrecondition", err)
	}

	// Repaired, it is served again without a restart.
	if err := os.WriteFile(journal, []byte("{\"op\":\"task\",\"id\":\"alpha/1\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.lead.Send(context.Background(), &relayv1.SendRequest{Agent: "alpha", Text: "repaired"}); err != nil {
		t.Errorf("a task after the repair: %v", err)
	}
	if _, err := r.lead.Cancel(context.Background(), &relayv1.CancelRequest{TaskId: "alpha/1"}); err != nil {
		t.Errorf("a cancel of the journal's task after the repair: %v", err)
	}
	e.mustConnect(r, "alpha", "shim-b")
	if r.h.isQuarantined("alpha") != nil {
		t.Error("still quarantined after the repair")
	}
}

// TestRuntimeBreak: a journal that breaks while the agent is attached
// quarantines it as one that cannot be opened: its stream ends, nothing
// more of it reaches the lead, the lead's calls for it are refused, it
// cannot connect again, and the lead's watches are told.
func TestRuntimeBreak(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	events, _ := watch(t, r, "beta")
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	a.send(question(task, "q1", "pending"))
	if q := next(t, events).GetQuestion(); q == nil {
		t.Fatal("no question")
	}
	st, _ := r.h.known("beta")
	st.mu.Lock()
	st.st.write = func([]byte) (int, error) { return 0, errors.New("disk full") }
	st.st.truncate = func(int64) error { return errors.New("read-only now") }
	st.mu.Unlock()
	a.send(message(task, "lead", "this breaks it"))
	if err := a.endErr(); status.Code(err) != codes.Unavailable {
		t.Errorf("the stream after the break: %v, want Unavailable", err)
	}
	var told bool
	for !told {
		ev := next(t, events)
		if ev.GetMessage() != nil || ev.GetTurnEvent() != nil {
			t.Errorf("after the break the lead got %v", ev)
		}
		told = strings.Contains(ev.GetDropped().GetReason(), "broken")
	}
	// Nothing more of it is taken, even what was already in flight.
	r.h.inbound(st, turn(task, "late output"))
	select {
	case ev := <-events:
		if ev.GetTurnEvent() != nil {
			t.Errorf("turn output of a broken agent reached the lead: %v", ev)
		}
	case <-time.After(100 * time.Millisecond):
	}
	queued := pending(r, "beta")
	for name, call := range map[string]func() error{
		"Send": func() error {
			_, err := r.lead.Send(context.Background(), &relayv1.SendRequest{Agent: "beta", Text: "x"})
			return err
		},
		"Reply": func() error {
			_, err := r.lead.Reply(context.Background(), &relayv1.ReplyRequest{QuestionId: "beta/q1", Text: "x"})
			return err
		},
		"Cancel": func() error {
			_, err := r.lead.Cancel(context.Background(), &relayv1.CancelRequest{TaskId: task})
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s for a broken agent: %v, want FailedPrecondition", name, err)
		}
	}
	if n := pending(r, "beta"); n != queued {
		t.Errorf("%d envelopes queued for a broken agent, was %d: a refused call queued one", n, queued)
	}
	if again, err := e.connect(r, "spiffe://"+td+"/agent/beta", "shim-1"); err == nil {
		if err := again.endErr(); status.Code(err) != codes.Unavailable {
			t.Errorf("a new stream of a broken agent: %v, want Unavailable", err)
		}
	}
}

// TestCompactionFailsAfterRename: atomicfile.Write failing after its
// rename leaves the path the new journal; appends go to it, not to the
// unlinked old one, and survive a reopen.
func TestCompactionFailsAfterRename(t *testing.T) {
	s := openTestStore(t)
	s.replace = func(path string, data []byte, mode os.FileMode) error {
		if err := atomicfile.Write(path, data, mode); err != nil {
			return err
		}
		return errors.New("syncing the directory failed")
	}
	before, err := s.f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if err := s.add(record{Op: "message", held: held{N: s.nextMsg, Text: "x"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.add(record{Op: "delivered", held: held{N: s.nextMsg - 1}}); err != nil {
			t.Fatal(err)
		}
		now, _ := s.f.Stat()
		if !os.SameFile(before, now) {
			break
		}
		if i > 10000 {
			t.Fatal("no compaction")
		}
	}
	if s.broken != nil {
		t.Fatalf("broken: %v", s.broken)
	}
	if err := s.add(record{Op: "task", held: held{ID: "a/after"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.sync(); err != nil {
		t.Fatal(err)
	}
	s.close()
	again, err := openStore(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.close()
	if !again.open("a/after", false) {
		t.Error("an append after a compaction that failed after its rename was lost")
	}
}

// TestAttachRefusesABrokenJournal: a journal that breaks after Connect's
// check, before the stream is attached, still refuses it: attach checks
// again under the lock the break is made under.
func TestAttachRefusesABrokenJournal(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	a, err := r.h.agent("beta")
	if err != nil {
		t.Fatal(err)
	}
	// The window: an older stream is attached, Connect has checked, and
	// the journal breaks while the older one is let go.
	oldCtx, releaseOld, err := a.attach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	attached := make(chan error, 1)
	go func() {
		_, release, err := a.attach(context.Background())
		if err == nil {
			release()
		}
		attached <- err
	}()
	// The second attach has replaced the older stream and waits for it.
	<-oldCtx.Done()
	a.mu.Lock()
	a.st.broken = errors.New("broken in the window")
	a.mu.Unlock()
	releaseOld()
	if err := <-attached; !errors.Is(err, errBroken) {
		t.Errorf("attach after the break: %v, want errBroken", err)
	}
	a.mu.Lock()
	cur := a.cur
	a.mu.Unlock()
	if cur != nil {
		t.Error("a stream of a broken agent is attached")
	}
}
