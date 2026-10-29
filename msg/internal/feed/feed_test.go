package feed

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/msg/internal/leadtest"
)

var small = Config{MaxBytes: 4 << 10, MaxEvents: 100, MaxHeld: 1 << 10, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}

// start runs a Feed of a fake whose first Watch reads the returned channel.
func start(t *testing.T, cfg Config) (*Feed, *leadtest.Fake, chan *relayv1.WatchEvent) {
	t.Helper()
	c, fake := leadtest.Client(t)
	ch := make(chan *relayv1.WatchEvent)
	fake.Watches <- ch
	f := New(c, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return f, fake, ch
}

// describe is an event for comparisons: a WatchEvent's text or chunk, or a
// marker's count.
func describe(e *Event) string {
	switch {
	case e.Ev == nil:
		return fmt.Sprintf("lost %s %d", e.Agent, e.Lost)
	case e.Ev.GetQuestion() != nil:
		return "q " + e.Ev.GetQuestion().GetId()
	case e.Ev.GetMessage() != nil:
		return "m " + e.Ev.GetMessage().GetText()
	default:
		return string(e.Ev.GetTurnEvent().GetChunk())
	}
}

// all polls everything after cursor, waiting up to wait for something.
func all(t *testing.T, f *Feed, agent string, cursor uint64, wait time.Duration) ([]string, uint64) {
	t.Helper()
	var got []string
	next, more := f.Poll(context.Background(), agent, Pos{N: cursor, Low: cursor}, wait, func(e *Event) bool {
		got = append(got, describe(e))
		return true
	})
	if more {
		t.Errorf("more with a take that takes everything")
	}
	return got, next.N
}

// waitFor polls from 0 without taking until n events are buffered.
func waitFor(t *testing.T, f *Feed, n uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		last := f.last
		f.mu.Unlock()
		if last >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d events buffered, want %d", last, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCursors: a poll returns what follows its cursor and the cursor after
// it; the next poll from there returns only what came since.
func TestCursors(t *testing.T) {
	f, _, ch := start(t, DefaultConfig)
	ch <- leadtest.Turn("a", "a/t1", "one")
	ch <- leadtest.Turn("a", "a/t1", "two")
	waitFor(t, f, 2)
	got, next := all(t, f, "", 0, 0)
	if strings.Join(got, ",") != "one,two" || next != 2 {
		t.Fatalf("got %q to %d", got, next)
	}
	if got, again := all(t, f, "", next, 0); len(got) != 0 || again != next {
		t.Errorf("nothing new: got %q to %d", got, again)
	}
	ch <- leadtest.Turn("a", "a/t1", "three")
	waitFor(t, f, 3)
	if got, _ := all(t, f, "", next, 0); strings.Join(got, ",") != "three" {
		t.Errorf("after the cursor: %q", got)
	}
	// A cursor past the newest (another process's) starts over.
	if got, _ := all(t, f, "", 99, 0); len(got) != 3 {
		t.Errorf("a cursor past the end: %q", got)
	}
}

// TestPollWaits: a poll with nothing to return waits for an event, and
// returns empty at its deadline.
func TestPollWaits(t *testing.T) {
	f, _, ch := start(t, DefaultConfig)
	go func() {
		time.Sleep(50 * time.Millisecond)
		ch <- leadtest.Turn("a", "a/t1", "late")
	}()
	if got, _ := all(t, f, "", 0, 5*time.Second); strings.Join(got, ",") != "late" {
		t.Errorf("got %q, want the event that came while waiting", got)
	}
	begin := time.Now()
	if got, _ := all(t, f, "", 1, 100*time.Millisecond); len(got) != 0 {
		t.Errorf("got %q, want nothing", got)
	}
	if d := time.Since(begin); d < 100*time.Millisecond {
		t.Errorf("returned after %v, before its wait", d)
	}
}

// TestAgentFilter: a poll of one agent passes over the others' events,
// which a poll of all still returns.
func TestAgentFilter(t *testing.T) {
	f, _, ch := start(t, DefaultConfig)
	ch <- leadtest.Turn("a", "a/t1", "a1")
	ch <- leadtest.Question("b", "b/q1", "b/t1", "which?")
	ch <- leadtest.Turn("a", "a/t1", "a2")
	waitFor(t, f, 3)
	got, next := all(t, f, "a", 0, 0)
	if strings.Join(got, ",") != "a1,a2" || next != 3 {
		t.Errorf("agent a: %q to %d", got, next)
	}
	if got, _ := all(t, f, "", 0, 0); strings.Join(got, ",") != "a1,q b/q1,a2" {
		t.Errorf("all: %q", got)
	}
}

// collect is a Take that takes everything into got.
func collect(got *[]string) Take {
	return func(e *Event) bool { *got = append(*got, describe(e)); return true }
}

// TestFilteredPos: a poll of one agent keeps Low before the first event
// of another it passed over; a poll of all from its Pos gets that event
// and not what the first returned; once it is returned, Low moves on.
func TestFilteredPos(t *testing.T) {
	f, _, ch := start(t, DefaultConfig)
	ch <- leadtest.Turn("a", "a/t1", "a1")
	ch <- leadtest.Question("b", "b/q1", "b/t1", "which?")
	ch <- leadtest.Turn("a", "a/t1", "a2")
	waitFor(t, f, 3)
	var got []string
	next, _ := f.Poll(context.Background(), "a", Pos{}, 0, collect(&got))
	if strings.Join(got, ",") != "a1,a2" || next != (Pos{N: 3, Low: 1}) {
		t.Fatalf("agent a: %q to %+v", got, next)
	}
	got = nil
	if all, _ := f.Poll(context.Background(), "", next, 0, collect(&got)); strings.Join(got, ",") != "q b/q1" || all != (Pos{N: 3, Low: 3}) {
		t.Errorf("all from a's Pos: %q to %+v, want b's question only", got, all)
	}
	ch <- leadtest.Turn("a", "a/t1", "a3")
	waitFor(t, f, 4)
	got = nil
	if again, _ := f.Poll(context.Background(), "a", next, 0, collect(&got)); strings.Join(got, ",") != "a3" || again != (Pos{N: 4, Low: 4}) {
		t.Errorf("agent a again: %q to %+v, want Low past the returned question", got, again)
	}
}

// TestLongFilteredRun: many polls of one agent, page by page, then one of
// all: only what the first passed over comes, not the agent's events again.
func TestLongFilteredRun(t *testing.T) {
	f, _, ch := start(t, DefaultConfig)
	ch <- leadtest.Question("b", "b/q1", "b/t1", "which?")
	for i := range 50 {
		ch <- leadtest.Turn("a", "a/t1", fmt.Sprint(i))
	}
	ch <- leadtest.Message("b", "b/t1", "later")
	waitFor(t, f, 52)
	pos, n := Pos{}, 0
	for range 100 {
		var page []string
		next, more := f.Poll(context.Background(), "a", pos, 0, func(e *Event) bool {
			if len(page) == 3 {
				return false
			}
			page = append(page, describe(e))
			return true
		})
		pos, n = next, n+len(page)
		if !more {
			break
		}
	}
	if n != 50 || pos.Low != 0 {
		t.Fatalf("%d events of a to %+v, want 50 and Low 0 (b's question unreturned)", n, pos)
	}
	var got []string
	if all, _ := f.Poll(context.Background(), "", pos, 0, collect(&got)); strings.Join(got, ",") != "q b/q1,m later" || all != (Pos{N: 52, Low: 52}) {
		t.Errorf("all after the run: %q to %+v, want b's two events only", got, all)
	}
}

// offline is a Feed without a Watch, filled by add.
func offline(t *testing.T, events ...*relayv1.WatchEvent) *Feed {
	t.Helper()
	f := New(nil, DefaultConfig)
	for _, ev := range events {
		if err := f.add(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// pageOf is a Take of n events at most into got.
func pageOf(n int, got *[]string) Take {
	return func(e *Event) bool {
		if len(*got) == n {
			return false
		}
		*got = append(*got, describe(e))
		return true
	}
}

// TestPagePassedOver: the review's case. Passed over by a poll of a, b's
// question and message come to polls of all one page at a time; the one a
// full page refuses comes next.
func TestPagePassedOver(t *testing.T) {
	f := offline(t,
		leadtest.Question("b", "b/q1", "b/t1", "which?"),
		leadtest.Turn("a", "a/t1", "a1"),
		leadtest.Message("b", "b/t1", "fyi"),
	)
	var got []string
	pos, _ := f.Poll(context.Background(), "a", Pos{}, 0, collect(&got))
	if strings.Join(got, ",") != "a1" || pos != (Pos{N: 3, Low: 0}) {
		t.Fatalf("agent a: %q to %+v", got, pos)
	}
	var pages []string
	for range 5 {
		var page []string
		next, more := f.Poll(context.Background(), "", pos, 0, pageOf(1, &page))
		pages = append(pages, strings.Join(page, ","))
		pos = next
		if !more && len(page) == 0 {
			break
		}
	}
	if strings.Join(pages, "|") != "q b/q1|m fyi|" || pos != (Pos{N: 3, Low: 3}) {
		t.Errorf("pages %q to %+v, want the question, the message, then nothing", pages, pos)
	}
	if f.heldSize != 0 {
		t.Errorf("%d bytes held after both were returned", f.heldSize)
	}
}

// TestEveryEventOnce: under random filters and page sizes, with events
// arriving between polls, one cursor passed from poll to poll returns
// every event exactly once.
func TestEveryEventOnce(t *testing.T) {
	agents := []string{"a", "b", "c"}
	for seed := range int64(300) {
		r := rand.New(rand.NewPCG(uint64(seed), 0))
		f := offline(t)
		seen := map[string]int{}
		n := 0
		event := func() {
			n++
			agent := agents[r.IntN(len(agents))]
			id := fmt.Sprintf("%s-%d", agent, n)
			var ev *relayv1.WatchEvent
			switch r.IntN(3) {
			case 0:
				ev = leadtest.Turn(agent, agent+"/t1", id)
			case 1:
				ev = leadtest.Question(agent, agent+"/"+id, agent+"/t1", "q")
			default:
				ev = leadtest.Message(agent, agent+"/t1", id)
			}
			if err := f.add(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		}
		key := func(e *Event) string {
			if q := e.Ev.GetQuestion(); q != nil {
				return q.GetId()
			}
			if m := e.Ev.GetMessage(); m != nil {
				return m.GetText()
			}
			return string(e.Ev.GetTurnEvent().GetChunk())
		}
		poll := func(pos Pos, agent string, size int) (Pos, bool, int) {
			taken := 0
			next, more := f.Poll(context.Background(), agent, pos, 0, func(e *Event) bool {
				if taken == size {
					return false
				}
				taken++
				seen[key(e)]++
				return true
			})
			return next, more, taken
		}
		var pos Pos
		for range 60 {
			for range r.IntN(3) {
				event()
			}
			filter := ""
			if r.IntN(3) > 0 {
				filter = agents[r.IntN(len(agents))]
			}
			pos, _, _ = poll(pos, filter, 1+r.IntN(4))
		}
		// Then polls of all until nothing more comes.
		for range n + 2 {
			var more bool
			var taken int
			pos, more, taken = poll(pos, "", 1+r.IntN(4))
			if !more && taken == 0 {
				break
			}
		}
		if len(seen) != n {
			t.Fatalf("seed %d: %d of %d events returned", seed, len(seen), n)
		}
		for k, c := range seen {
			if c != 1 {
				t.Fatalf("seed %d: %s returned %d times", seed, k, c)
			}
		}
		if f.heldSize != 0 {
			t.Fatalf("seed %d: %d bytes held after every event was returned", seed, f.heldSize)
		}
	}
}

// TestHeldSurviveAFullBuffer: turn output past the bounds goes, oldest
// first, counted by one marker per agent; questions and messages stay
// until a poll returns them, and may go after.
func TestHeldSurviveAFullBuffer(t *testing.T) {
	f, _, ch := start(t, small)
	ch <- leadtest.Question("a", "a/q1", "a/t1", "which?")
	ch <- leadtest.Message("a", "a/t1", "fyi")
	big := strings.Repeat("x", 1000)
	for range 20 {
		ch <- leadtest.Turn("a", "a/t1", big)
	}
	ch <- leadtest.Turn("a", "a/t1", "last")
	waitFor(t, f, 23)
	got, _ := all(t, f, "", 0, 0)
	if len(got) < 4 || got[0] != "q a/q1" || got[1] != "m fyi" || !strings.HasPrefix(got[2], "lost a ") || got[len(got)-1] != "last" {
		t.Fatalf("got %q, want the question, the message, a marker, then the newest output", got)
	}
	var lost int
	if _, err := fmt.Sscanf(got[2], "lost a %d", &lost); err != nil || lost+len(got)-3 != 21 {
		t.Errorf("marker %q and %d events of output kept, want 21 in all", got[2], len(got)-3)
	}
	// Returned, they may go: more output pushes them out.
	for range 10 {
		ch <- leadtest.Turn("a", "a/t1", big)
	}
	waitFor(t, f, 33)
	got, _ = all(t, f, "", 0, 0)
	if got[0] == "q a/q1" || got[1] == "m fyi" {
		t.Errorf("returned questions and messages kept past the bound: %q", got[:2])
	}
}

// TestHeldWaitsForAPoll: past MaxHeld of unreturned questions and
// messages, the feed stops reading the Watch until a poll returns some;
// nothing is dropped.
func TestHeldWaitsForAPoll(t *testing.T) {
	f, _, ch := start(t, small)
	text := strings.Repeat("m", 600)
	ch <- leadtest.Message("a", "a/t1", text+"1")
	sent := make(chan struct{})
	go func() {
		ch <- leadtest.Message("a", "a/t1", text+"2")
		ch <- leadtest.Message("a", "a/t1", text+"3")
		close(sent)
	}()
	waitFor(t, f, 1)
	// The fake sends them (gRPC's window holds them), and the feed does not
	// take them.
	<-sent
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	last := f.last
	f.mu.Unlock()
	if last != 1 {
		t.Fatalf("the feed took %d messages, past MaxHeld, without a poll", last)
	}
	var got []string
	cursor := uint64(0)
	for len(got) < 3 {
		var page []string
		page, cursor = all(t, f, "", cursor, 5*time.Second)
		got = append(got, page...)
	}
	for i, g := range got {
		if g != "m "+text+fmt.Sprint(i+1) {
			t.Errorf("message %d: %.20q...", i+1, g)
		}
	}
}

// TestReconnect: a broken Watch is redialled; the pending questions the
// relay replays to the new one are not taken twice, and what is new is.
func TestReconnect(t *testing.T) {
	f, fake, ch := start(t, small)
	ch <- leadtest.Question("a", "a/q1", "a/t1", "which?")
	waitFor(t, f, 1)
	if s := f.State(); s != "connected" {
		t.Errorf("state %q", s)
	}
	close(ch)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasPrefix(f.State(), "reconnecting: ") {
		if time.Now().After(deadline) {
			t.Fatalf("state %q after the watch broke", f.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
	ch2 := make(chan *relayv1.WatchEvent)
	fake.Watches <- ch2
	ch2 <- leadtest.Question("a", "a/q1", "a/t1", "which?")
	ch2 <- leadtest.Question("a", "a/q2", "a/t1", "and?")
	waitFor(t, f, 2)
	if got, _ := all(t, f, "", 0, 0); strings.Join(got, ",") != "q a/q1,q a/q2" {
		t.Errorf("got %q, want each question once", got)
	}
	if s := f.State(); s != "connected" {
		t.Errorf("state %q after the redial", s)
	}
	if n := len(fake.Got()); n != 2 {
		t.Errorf("%d watches, want 2", n)
	}
}

// TestPaging: a take that refuses leaves the event for the next poll.
func TestPaging(t *testing.T) {
	f, _, ch := start(t, DefaultConfig)
	for i := range 5 {
		ch <- leadtest.Turn("a", "a/t1", fmt.Sprint(i))
	}
	waitFor(t, f, 5)
	var got []string
	cursor := uint64(0)
	for range 10 {
		var page []string
		next, more := f.Poll(context.Background(), "", Pos{N: cursor, Low: cursor}, 0, func(e *Event) bool {
			if len(page) == 2 {
				return false
			}
			page = append(page, describe(e))
			return true
		})
		got, cursor = append(got, page...), next.N
		if !more {
			break
		}
	}
	if strings.Join(got, ",") != "0,1,2,3,4" {
		t.Errorf("paged %q", got)
	}
}
