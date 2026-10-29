package hub

import (
	"fmt"
	"strings"
	"unicode/utf8"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
)

// dropKind is what an inbound drop counts as.
type dropKind int

const (
	dropTurn dropKind = iota
	dropQuestion
	dropMessage
	dropInvalid
)

// inbound takes one envelope from the agent (Session.Handle). Whatever it
// does not take is dropped and counted (drop), never an error: the envelope
// is acked either way, so a hostile agent cannot make the relay hold it.
func (h *Hub) inbound(a *agentState, e *relayv1.Envelope) {
	if a.journalBroken() {
		// Quarantined: nothing more of it is taken.
		h.drop(a, dropInvalid, "the relay's journal of this agent is broken")
		return
	}
	var kind dropKind
	var reason string
	switch e.WhichBody() {
	case relayv1.Envelope_TurnEvent_case:
		kind, reason = dropTurn, h.turnEvent(a, e.GetTurnEvent())
	case relayv1.Envelope_Question_case:
		kind, reason = dropQuestion, h.question(a, e.GetQuestion())
	case relayv1.Envelope_Message_case:
		kind, reason = dropMessage, h.message(a, e.GetMessage())
	default:
		kind, reason = dropInvalid, fmt.Sprintf("an envelope an agent does not send (body field %d)", e.WhichBody())
	}
	if reason != "" {
		h.drop(a, kind, reason)
	}
}

// drop counts a dropped envelope, and tells the lead's watches at most every
// markerEvery.
func (h *Hub) drop(a *agentState, kind dropKind, reason string) {
	a.mu.Lock()
	if a.drops == nil {
		a.drops = relayv1.Dropped_builder{}.Build()
	}
	switch kind {
	case dropTurn:
		a.drops.SetTurnEvents(a.drops.GetTurnEvents() + 1)
	case dropQuestion:
		a.drops.SetQuestions(a.drops.GetQuestions() + 1)
	case dropMessage:
		a.drops.SetMessages(a.drops.GetMessages() + 1)
	default:
		a.drops.SetInvalid(a.drops.GetInvalid() + 1)
	}
	a.drops.SetReason(reason)
	var d *relayv1.Dropped
	if now := h.now(); now.Sub(a.lastMarker) >= markerEvery {
		d, a.drops, a.lastMarker = a.drops, nil, now
	}
	a.mu.Unlock()
	if d != nil {
		a.log.Warn("dropped envelopes from the agent", "turn_events", d.GetTurnEvents(), "questions", d.GetQuestions(),
			"messages", d.GetMessages(), "invalid", d.GetInvalid(), "reason", d.GetReason())
		h.broadcast(relayv1.WatchEvent_builder{Agent: a.name, Dropped: d}.Build(), note, 0)
	}
}

// taskOpen is "" when task is one the relay gave this agent and has not
// closed (or, for turn output, has cancelled and not seen end), and why not
// otherwise; a.mu is held.
func (a *agentState) taskOpen(task string, turn bool) string {
	if !a.st.open(task, turn) {
		return fmt.Sprintf("task %q is not an open task of this agent", truncate(task, 80))
	}
	return ""
}

// text is s valid as UTF-8, cut to at most n bytes.
func text(s string, n int) string {
	return truncate(strings.ToValidUTF8(s, "�"), n)
}

// truncate cuts s to at most n bytes, on a rune boundary, marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = " [cut]"
	cut := n - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// turnEvent takes turn output; the last event closes the task. The last
// event is not rate limited: a task must be able to end.
func (h *Hub) turnEvent(a *agentState, ev *relayv1.TurnEvent) string {
	if len(ev.GetChunk()) > maxChunk {
		return "turn output over its size limit"
	}
	if len(ev.GetError()) > maxError {
		// Cut, not dropped: a task must be able to end, and a last event
		// dropped would leave it open for good.
		ev.SetError(truncate(ev.GetError(), maxError))
	}
	a.mu.Lock()
	if why := a.taskOpen(ev.GetTaskId(), true); why != "" {
		a.mu.Unlock()
		return why
	}
	now := h.now()
	if !ev.GetDone() && (!a.turnEvents.take(1, now) || !a.turnBytes.take(float64(len(ev.GetChunk())), now)) {
		a.mu.Unlock()
		return "turn output over its rate"
	}
	c := turnOutput
	if ev.GetDone() {
		c = turnEnd
		_ = a.record(record{Op: "close", held: held{ID: ev.GetTaskId()}})
	}
	a.mu.Unlock()
	h.broadcast(relayv1.WatchEvent_builder{Agent: a.name, TurnEvent: ev}.Build(), c, 0)
	return ""
}

// question takes a question: a valid id, a bounded text, an open task, under
// the agent's count and rate.
func (h *Hub) question(a *agentState, q *relayv1.Question) string {
	switch {
	case !questionID.MatchString(q.GetId()):
		return "a question id that is not one"
	case q.GetText() == "" || len(q.GetText()) > maxQuestionText:
		return "a question without a text or over its size limit"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if why := a.taskOpen(q.GetTaskId(), false); why != "" {
		return why
	}
	// The same question again (a resend the record could not catch, such
	// as one after a crash before the ack) is the pending one.
	if _, ok := a.st.question(q.GetId()); ok {
		return ""
	}
	if len(a.st.questions) >= maxQuestions {
		return fmt.Sprintf("over %d pending questions", maxQuestions)
	}
	if !a.notes.take(1, h.now()) {
		return "questions and messages over their rate"
	}
	pq := held{ID: q.GetId(), Text: q.GetText(), TaskID: q.GetTaskId()}
	if a.record(record{Op: "question", held: pq}) != nil {
		return "the journal could not be written"
	}
	h.broadcast(questionEvent(a.name, pq), note, 0)
	return ""
}

// message takes a message for the lead: a bounded text, an open task, under
// the agent's rate and the held bytes, the agent's and everyone's.
func (h *Hub) message(a *agentState, m *relayv1.Message) string {
	switch {
	case m.GetTo() != "lead":
		return "a message not for the lead: messages go to the lead only"
	case m.GetText() == "" || len(m.GetText()) > maxMessageText:
		return "a message without a text or over its size limit"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if why := a.taskOpen(m.GetTaskId(), false); why != "" {
		return why
	}
	size := heldSize(m.GetText())
	switch {
	case len(a.st.messages) >= maxHeldMessages:
		return fmt.Sprintf("over %d held messages", maxHeldMessages)
	case a.st.msgBytes+size > maxHeldBytes:
		return "the agent's held messages are over their size limit"
	case h.held.Load()+int64(size) > maxHeldTotal:
		return "the held messages of all agents are over their size limit"
	case !a.notes.take(1, h.now()):
		return "questions and messages over their rate"
	}
	msg := held{N: a.st.nextMsg, Text: m.GetText(), TaskID: m.GetTaskId()}
	if a.record(record{Op: "message", held: msg}) != nil {
		return "the journal could not be written"
	}
	h.held.Add(int64(size))
	h.broadcast(messageEvent(a.name, msg), note, msg.N)
	return ""
}
