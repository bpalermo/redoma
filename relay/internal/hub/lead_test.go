package hub

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
)

// give sends the agent a task through the lead and returns its id, once the
// agent has it.
func give(t *testing.T, r *relay, a *agent, agentName, text string) string {
	t.Helper()
	sent, err := r.lead.Send(context.Background(), relayv1.SendRequest_builder{Agent: agentName, Text: text}.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := a.recv(); got.GetTask().GetId() != sent.GetTaskId() {
		t.Fatalf("got %v, want task %s", got, sent.GetTaskId())
	}
	return sent.GetTaskId()
}

// clock is a test's time: it moves only when told, or by step at every
// reading when step is set.
type clock struct {
	t    atomic.Int64
	step atomic.Int64
}

func setClock(r *relay) *clock {
	c := &clock{}
	c.t.Store(time.Now().UnixNano())
	f := func() time.Time { return time.Unix(0, c.t.Add(c.step.Load())) }
	r.h.clock.Store(&f)
	return c
}

func (c *clock) add(d time.Duration) { c.t.Add(int64(d)) }

func turn(task, chunk string) *relayv1.Envelope {
	return relayv1.Envelope_builder{TurnEvent: relayv1.TurnEvent_builder{TaskId: task, Chunk: []byte(chunk)}.Build()}.Build()
}

func question(task, id, text string) *relayv1.Envelope {
	return relayv1.Envelope_builder{Question: relayv1.Question_builder{Id: id, Text: text, TaskId: task}.Build()}.Build()
}

func message(task, to, text string) *relayv1.Envelope {
	return relayv1.Envelope_builder{Message: relayv1.Message_builder{To: to, Text: text, TaskId: task}.Build()}.Build()
}

func doneOf(task string) *relayv1.Envelope {
	return relayv1.Envelope_builder{TurnEvent: relayv1.TurnEvent_builder{TaskId: task, Done: true}.Build()}.Build()
}

// dropped is the next event, which must be a Dropped.
func dropped(t *testing.T, c <-chan *relayv1.WatchEvent) *relayv1.Dropped {
	t.Helper()
	ev := next(t, c)
	if ev.GetDropped() == nil {
		t.Fatalf("%v, want a Dropped", ev)
	}
	return ev.GetDropped()
}

// TestTaskQueuedThenDelivered: a task sent while the agent is away is
// delivered once it connects, and leaves the queue once acked.
func TestTaskQueuedThenDelivered(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	sent, err := r.lead.Send(context.Background(), relayv1.SendRequest_builder{Agent: "beta", Text: "fix the build"}.Build())
	if err != nil {
		t.Fatal(err)
	}
	a := e.mustConnect(r, "beta", "shim-1")
	if a.relay == "" {
		t.Error("no relay epoch in the response header")
	}
	got := a.recv()
	if got.GetTask().GetId() != sent.GetTaskId() || got.GetTask().GetText() != "fix the build" || got.GetSeq() != 1 {
		t.Errorf("got %v, want task %s", got, sent.GetTaskId())
	}
	a.ack(got.GetSeq())
	eventually(t, "the ack to empty the queue", func() bool { return pending(r, "beta") == 0 })
}

// TestAgentToLead: what the agent sends for its task reaches the lead's
// watch; a reply reaches the agent and answers the question; the task's end
// closes it.
func TestAgentToLead(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	events, _ := watch(t, r, "")
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")

	a.send(turn(task, `{"type":"assistant"}`))
	if ev := next(t, events); ev.GetAgent() != "beta" || string(ev.GetTurnEvent().GetChunk()) != `{"type":"assistant"}` {
		t.Errorf("turn event: %v", ev)
	}
	a.send(question(task, "q1", "which branch?"))
	q := next(t, events).GetQuestion()
	if q.GetId() != "beta/q1" || q.GetText() != "which branch?" || q.GetTaskId() != task {
		t.Errorf("question: %v", q)
	}
	a.send(message(task, "lead", "done soon"))
	if m := next(t, events).GetMessage(); m.GetText() != "done soon" {
		t.Errorf("message: %v", m)
	}
	if _, err := r.lead.Reply(context.Background(), relayv1.ReplyRequest_builder{QuestionId: q.GetId(), Text: "main"}.Build()); err != nil {
		t.Fatal(err)
	}
	if got := a.recv().GetReply(); got.GetQuestionId() != "q1" || got.GetText() != "main" {
		t.Errorf("reply: %v", got)
	}
	if _, err := r.lead.Reply(context.Background(), relayv1.ReplyRequest_builder{QuestionId: q.GetId(), Text: "again"}.Build()); status.Code(err) != codes.NotFound {
		t.Errorf("a second reply: %v, want NotFound", err)
	}
	a.send(doneOf(task))
	if ev := next(t, events); !ev.GetTurnEvent().GetDone() {
		t.Errorf("the end: %v", ev)
	}
	// Closed: nothing more of it is taken.
	a.send(turn(task, "after the end"))
	if d := dropped(t, events); d.GetInvalid()+d.GetTurnEvents() != 1 || !strings.Contains(d.GetReason(), "not an open task") {
		t.Errorf("output after the end: %v", d)
	}
	eventually(t, "the agent's envelopes to be saved", func() bool {
		st, _ := r.h.known("beta")
		return st.in.Saved("shim-1") == 5
	})
}

// TestForgedTasks: an agent cannot speak for a task the relay did not give
// it: another agent's, an invented one, none.
func TestForgedTasks(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	events, _ := watch(t, r, "")
	b := e.mustConnect(r, "alpha", "shim-b")
	theirs := give(t, r, b, "alpha", "theirs")
	a := e.mustConnect(r, "beta", "shim-a")
	mine := give(t, r, a, "beta", "mine")

	for _, task := range []string{theirs, "beta/invented", "", "alpha/" + strings.Repeat("x", 1000)} {
		a.send(turn(task, "forged"))
		a.send(question(task, "q1", "forged?"))
		a.send(message(task, "lead", "forged"))
	}
	// Turn output and notes of the forged tasks are all dropped; the
	// first drop is told at once, the rest coalesced into the next marker.
	d := dropped(t, events)
	if !strings.Contains(d.GetReason(), "not an open task") {
		t.Errorf("first marker %v", d)
	}
	sa, _ := r.h.known("beta")
	eventually(t, "every forged envelope taken", func() bool { return sa.in.Saved("shim-a") == 12 })
	none(t, events)
	// The agent's own task still works, and b's is untouched.
	setClock(r).add(time.Hour)
	a.send(turn(mine, "real"))
	if ev := next(t, events); string(ev.GetTurnEvent().GetChunk()) != "real" {
		t.Errorf("the agent's own task: %v", ev)
	}
	st, _ := r.h.known("alpha")
	st.mu.Lock()
	open := st.st.open(theirs, false)
	st.mu.Unlock()
	if !open {
		t.Error("another agent's forged events touched its task")
	}
}

// TestMessagesOnlyToTheLead: a message for anyone else is dropped.
func TestMessagesOnlyToTheLead(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	events, _ := watch(t, r, "")
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	a.send(message(task, "alpha", "psst"))
	if d := dropped(t, events); d.GetMessages() != 1 || !strings.Contains(d.GetReason(), "lead only") {
		t.Errorf("a message to another agent: %v", d)
	}
}

// TestOversize: envelopes over their size limits are dropped; one over the
// server's message size never reaches the hub, and ends the stream.
func TestOversize(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	events, _ := watch(t, r, "")
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	c := setClock(r)
	for _, env := range []*relayv1.Envelope{
		turn(task, strings.Repeat("x", maxChunk+1)),
		question(task, "q1", strings.Repeat("x", maxQuestionText+1)),
		question(task, strings.Repeat("q", 65), "a long id"),
		question(task, "", "no id"),
		question(task, "a/b", "a slash"),
		message(task, "lead", strings.Repeat("x", maxMessageText+1)),
		message(task, "lead", ""),
	} {
		a.send(env)
		if d := dropped(t, events); !strings.Contains(d.GetReason(), "limit") && !strings.Contains(d.GetReason(), "not one") && !strings.Contains(d.GetReason(), "without") {
			t.Errorf("%v: %v", env.WhichBody(), d)
		}
		c.add(2 * markerEvery)
	}
	st, _ := r.h.known("beta")
	st.mu.Lock()
	n := len(st.st.questions) + len(st.st.messages)
	st.mu.Unlock()
	if n != 0 {
		t.Errorf("%d oversize questions and messages kept", n)
	}

	// Over the server's limit: the stream ends, nothing is handled.
	big := relayv1.Envelope_builder{Seq: 999, Message: relayv1.Message_builder{To: "lead", Text: strings.Repeat("x", MaxRecvMsgSize), TaskId: task}.Build()}.Build()
	_ = a.st.Send(big)
	if err := a.endErr(); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("an envelope over MaxRecvMsgSize: %v, want ResourceExhausted", err)
	}
}

// TestQuestionFlood: an agent cannot have more than maxQuestions pending,
// nor ask faster than its rate.
func TestQuestionFlood(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	c := setClock(r)
	// The burst, then nothing until time passes.
	for i := range notesBurst + 5 {
		a.send(question(task, "q"+strings.Repeat("x", i), "q"))
	}
	st, _ := r.h.known("beta")
	count := func() int {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.st.questions)
	}
	eventually(t, "the burst", func() bool { return st.in.Saved("shim-1") == uint64(notesBurst+5) })
	if n := count(); n != notesBurst {
		t.Errorf("%d questions taken in a burst, want %d", n, notesBurst)
	}
	// With time, the rate refills, up to the count cap.
	for i := range 2 * maxQuestions {
		c.add(time.Second)
		a.send(question(task, "r"+strings.Repeat("x", i), "q"))
	}
	eventually(t, "the rest", func() bool { return st.in.Saved("shim-1") == uint64(notesBurst+5+2*maxQuestions) })
	if n := count(); n != maxQuestions {
		t.Errorf("%d questions pending, want the cap, %d", n, maxQuestions)
	}
}

// TestHeldMessages: a message that arrives while nobody watches is held and
// given to the next watch, once; held messages are capped in bytes per
// agent.
func TestHeldMessages(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	setClock(r).step.Store(int64(time.Second))
	text := strings.Repeat("m", maxMessageText)
	n := maxHeldBytes/maxMessageText + 3
	for range n {
		a.send(message(task, "lead", text))
	}
	st, _ := r.h.known("beta")
	eventually(t, "the messages", func() bool { return st.in.Saved("shim-1") == uint64(n) })
	st.mu.Lock()
	held, bytes := len(st.st.messages), st.st.msgBytes
	st.mu.Unlock()
	if bytes > maxHeldBytes || held != maxHeldBytes/heldSize(text) {
		t.Errorf("%d messages, %d bytes held; want the cap, %d bytes", held, bytes, maxHeldBytes)
	}
	if r.h.held.Load() != int64(bytes) {
		t.Errorf("the total held is %d, want %d", r.h.held.Load(), bytes)
	}

	events, cancel := watch(t, r, "")
	got := 0
	for range held {
		ev := next(t, events)
		if ev.GetMessage() != nil {
			got++
		}
	}
	if got != held {
		t.Errorf("replayed %d messages, want %d", got, held)
	}
	eventually(t, "the delivered messages to leave", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.st.messages) == 0
	})
	cancel()
	events, _ = watch(t, r, "")
	for {
		select {
		case ev := <-events:
			if ev.GetMessage() != nil {
				t.Fatalf("a delivered message again: %v", ev)
			}
			continue
		case <-time.After(100 * time.Millisecond):
		}
		break
	}
	if r.h.held.Load() != 0 {
		t.Errorf("the total held is %d after delivery", r.h.held.Load())
	}
}

// TestHeldTotal: across agents, held messages stop at maxHeldTotal.
func TestHeldTotal(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	r.h.held.Store(maxHeldTotal - 10)
	events, _ := watch(t, r, "alpha") // a watch of someone else: nothing delivered
	_ = events
	a.send(message(task, "lead", "eleven bytes"))
	st, _ := r.h.known("beta")
	eventually(t, "the message", func() bool { return st.in.Saved("shim-1") == 1 })
	st.mu.Lock()
	n := len(st.st.messages)
	st.mu.Unlock()
	if n != 0 {
		t.Error("a message over the total was held")
	}
}

// TestTurnFlood: an agent flooding turn output hits its rate and is told
// with a Dropped; a watch that falls behind one agent drops that agent's
// turn output only, never another agent's events nor its notes.
func TestTurnFlood(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	a := e.mustConnect(r, "beta", "shim-a")
	flood := give(t, r, a, "beta", "flood")
	b := e.mustConnect(r, "alpha", "shim-b")
	quiet := give(t, r, b, "alpha", "quiet")

	// A watch nobody reads yet: its queues fill.
	w := r.h.watch("")
	defer r.h.unwatch(w)
	chunk := strings.Repeat("x", 64<<10)
	n := watchTurnBytes/len(chunk) + 20
	for range n {
		a.send(turn(flood, chunk))
	}
	b.send(turn(quiet, "b's line"))
	b.send(question(quiet, "q1", "b's question"))
	a.send(question(flood, "q1", "a's question"))
	sa, _ := r.h.known("beta")
	sb, _ := r.h.known("alpha")
	eventually(t, "everything taken", func() bool {
		return sa.in.Saved("shim-a") == uint64(n+1) && sb.in.Saved("shim-b") == 2
	})

	var got []*relayv1.WatchEvent
	for {
		it, ok := w.pop()
		if !ok {
			break
		}
		got = append(got, it.ev)
	}
	var aTurns, aDropped uint64
	var bLine, bQuestion, aQuestion bool
	for _, ev := range got {
		switch {
		case ev.GetAgent() == "beta" && ev.GetTurnEvent() != nil:
			aTurns++
		case ev.GetAgent() == "beta" && ev.GetDropped() != nil:
			aDropped += ev.GetDropped().GetTurnEvents()
		case ev.GetAgent() == "beta" && ev.GetQuestion() != nil:
			aQuestion = true
		case ev.GetAgent() == "alpha" && ev.GetTurnEvent() != nil:
			bLine = true
		case ev.GetAgent() == "alpha" && ev.GetQuestion() != nil:
			bQuestion = true
		case ev.GetAgent() == "alpha" && ev.GetDropped() != nil:
			t.Errorf("b's events dropped: %v", ev)
		}
	}
	if !bLine || !bQuestion {
		t.Errorf("b's line %v, question %v: a's flood took them", bLine, bQuestion)
	}
	if !aQuestion {
		t.Error("a's question was dropped with its turn output")
	}
	if aDropped == 0 || aTurns+aDropped != uint64(n) {
		t.Errorf("a: %d turn events queued, %d dropped, of %d", aTurns, aDropped, n)
	}
	// The order is fair: b's events are not behind all of a's.
	for i, ev := range got {
		if ev.GetAgent() == "alpha" && i > 6 {
			t.Errorf("b's first event at %d, behind a's backlog", i)
		}
		if ev.GetAgent() == "alpha" {
			break
		}
	}
}

// TestTurnRate: past the burst, turn output is dropped until the rate
// refills; the task's end is never dropped.
func TestTurnRate(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	events, _ := watch(t, r, "")
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	setClock(r)
	for range turnEventsBurst + 1 {
		a.send(turn(task, "x"))
	}
	a.send(doneOf(task))
	var turns, drops int
	var done bool
	for !done {
		ev := next(t, events)
		switch {
		case ev.GetTurnEvent().GetDone():
			done = true
		case ev.GetTurnEvent() != nil:
			turns++
		case ev.GetDropped() != nil:
			drops += int(ev.GetDropped().GetTurnEvents())
		}
	}
	if turns != turnEventsBurst {
		t.Errorf("%d turn events through, want the burst, %d", turns, turnEventsBurst)
	}
	if drops != 1 {
		t.Errorf("%d dropped, want 1", drops)
	}
}

// TestCancel: a cancel goes to the task's agent, which may still end it but
// not ask about it.
func TestCancel(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	events, _ := watch(t, r, "")
	a := e.mustConnect(r, "alpha", "shim-1")
	task := give(t, r, a, "alpha", "go")
	if _, err := r.lead.Cancel(context.Background(), relayv1.CancelRequest_builder{TaskId: task}.Build()); err != nil {
		t.Fatal(err)
	}
	if got := a.recv().GetCancel(); got.GetTaskId() != task {
		t.Errorf("cancel: %v", got)
	}
	a.send(question(task, "q1", "still there?"))
	if d := dropped(t, events); d.GetQuestions() != 1 {
		t.Errorf("a question of a cancelled task: %v", d)
	}
	a.send(doneOf(task))
	if ev := next(t, events); !ev.GetTurnEvent().GetDone() {
		t.Errorf("the cancelled turn's end: %v", ev)
	}
	for _, id := range []string{"abc", "alpha/", "no-such-agent/abc", "alpha/never-sent", task} {
		if _, err := r.lead.Cancel(context.Background(), relayv1.CancelRequest_builder{TaskId: id}.Build()); err == nil {
			t.Errorf("cancel %q: accepted", id)
		}
	}
}

// TestOpenTaskCap: the lead may have maxOpenTasks open with an agent.
func TestOpenTaskCap(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	for range maxOpenTasks {
		if _, err := r.lead.Send(context.Background(), relayv1.SendRequest_builder{Agent: "beta", Text: "x"}.Build()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.lead.Send(context.Background(), relayv1.SendRequest_builder{Agent: "beta", Text: "x"}.Build()); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("one task too many: %v, want ResourceExhausted", err)
	}
}

// TestLeadRefusals: the lead can only task an agent of the allowlist, with
// a text.
func TestLeadRefusals(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	for name, req := range map[string]*relayv1.SendRequest{
		"not in the allowlist": relayv1.SendRequest_builder{Agent: "someone-else", Text: "x"}.Build(),
		"not a name":           relayv1.SendRequest_builder{Agent: "../etc", Text: "x"}.Build(),
		"no text":              relayv1.SendRequest_builder{Agent: "beta"}.Build(),
	} {
		if _, err := r.lead.Send(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := r.lead.Reply(context.Background(), relayv1.ReplyRequest_builder{QuestionId: "beta/nope", Text: "x"}.Build()); status.Code(err) != codes.NotFound {
		t.Errorf("a reply to no question: %v, want NotFound", err)
	}
}

// TestQueueSurvivesRestart: tasks queued, tasks open and questions pending
// are there after the relay restarts, and an envelope delivered before the
// restart but not acked is delivered again.
func TestQueueSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	a := e.mustConnect(r, "beta", "shim-1")
	first := give(t, r, a, "beta", "one")
	a.send(question(first, "q1", "why?"))
	st, _ := r.h.known("beta")
	eventually(t, "the question to be saved", func() bool { return st.in.Saved("shim-1") == 1 })
	// The task is not acked: the restart comes first.
	a.cancel()
	second, err := r.lead.Send(context.Background(), relayv1.SendRequest_builder{Agent: "beta", Text: "two"}.Build())
	if err != nil {
		t.Fatal(err)
	}
	r.stop()

	r = e.start()
	events, _ := watch(t, r, "beta")
	if q := next(t, events).GetQuestion(); q.GetId() != "beta/q1" {
		t.Errorf("after the restart the watch replays %v, want the pending question", q)
	}
	a = e.mustConnect(r, "beta", "shim-1")
	if got := a.recv(); got.GetTask().GetId() != first || got.GetSeq() != 1 {
		t.Errorf("first after the restart: %v, want %s again (seq 1)", got, first)
	}
	if got := a.recv(); got.GetTask().GetId() != second.GetTaskId() || got.GetSeq() != 2 {
		t.Errorf("second after the restart: %v, want %s (seq 2)", got, second.GetTaskId())
	}
	a.ack(2)
	eventually(t, "the queue to empty", func() bool { return pending(r, "beta") == 0 })
	// Seq 1 of shim-1 was taken before the restart: whatever comes again
	// under it is dropped, unseen.
	a.seq = 0
	a.send(question(first, "q2", "a resend"))
	none(t, events)
	// The tasks are still open.
	a.send(turn(second.GetTaskId(), "two's output"))
	if ev := next(t, events); string(ev.GetTurnEvent().GetChunk()) != "two's output" {
		t.Errorf("the second task after the restart: %v", ev)
	}
}

// TestLongDoneErrorIsCut: a last event with a long error is cut, not
// dropped: the task still ends.
func TestLongDoneErrorIsCut(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	events, _ := watch(t, r, "")
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	a.send(relayv1.Envelope_builder{TurnEvent: relayv1.TurnEvent_builder{TaskId: task, Done: true, Error: strings.Repeat("€", maxError)}.Build()}.Build())
	ev := next(t, events).GetTurnEvent()
	// 3-byte runes: the cut falls inside one, and backs up to its start.
	if !ev.GetDone() || len(ev.GetError()) > maxError || !strings.HasSuffix(ev.GetError(), "[cut]") || !utf8.ValidString(ev.GetError()) {
		t.Errorf("the end: done %v, error of %d bytes, valid %v", ev.GetDone(), len(ev.GetError()), utf8.ValidString(ev.GetError()))
	}
	st, _ := r.h.known("beta")
	st.mu.Lock()
	open := st.st.open(task, true)
	st.mu.Unlock()
	if open {
		t.Error("the task is still open")
	}
}

// TestHeldCount: tiny messages stop at maxHeldMessages, however few bytes
// they hold.
func TestHeldCount(t *testing.T) {
	e := newEnv(t)
	r := e.start()
	a := e.mustConnect(r, "beta", "shim-1")
	task := give(t, r, a, "beta", "go")
	setClock(r).step.Store(int64(time.Second))
	n := maxHeldMessages + 10
	for range n {
		a.send(message(task, "lead", "x"))
	}
	st, _ := r.h.known("beta")
	eventually(t, "the messages", func() bool { return st.in.Saved("shim-1") == uint64(n) })
	st.mu.Lock()
	held := len(st.st.messages)
	st.mu.Unlock()
	if held != maxHeldMessages {
		t.Errorf("%d held, want %d", held, maxHeldMessages)
	}
}

// TestTruncate: a cut inside a rune backs up to its start, wherever in the
// rune it falls; invalid bytes are made valid first (text).
func TestTruncate(t *testing.T) {
	const mark = " [cut]"
	for pad := range 4 {
		s := strings.Repeat("a", pad) + strings.Repeat("€", 100)
		got := truncate(s, 50)
		if len(got) > 50 || !utf8.ValidString(got) || !strings.HasSuffix(got, mark) {
			t.Errorf("pad %d: %q (%d bytes)", pad, got, len(got))
		}
	}
	if got := truncate("short", 50); got != "short" {
		t.Errorf("a short string: %q", got)
	}
	if got := text("bad \xff\xfe \xf0\x9f\x98", 50); !utf8.ValidString(got) {
		t.Errorf("invalid bytes: %q", got)
	}
}
