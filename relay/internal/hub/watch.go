package hub

import (
	"context"
	"sync"

	"google.golang.org/protobuf/proto"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
)

// watcher is one Watch. It keeps a queue per agent and sends from them in
// turn, so one agent's flood delays the others by one event at a time, not
// by its whole backlog. In each agent's queue, notes (questions and
// messages, bounded by the store and the rate limits) come before turn
// output, and are never dropped to make room for it; turn output past
// watchTurnBytes is dropped, that agent's alone, and the watch is told with
// a Dropped. A watcher is never cut off.
type watcher struct {
	agent string

	mu     sync.Mutex
	queues map[string]*wqueue
	order  []string
	next   int
	wake   chan struct{}
}

type witem struct {
	ev *relayv1.WatchEvent
	// msg, for a message, is its number: delivered once sent.
	msg  uint64
	size int
}

type wqueue struct {
	notes     []witem
	noteBytes int
	turn      []witem
	turnBytes int
	dropped   *relayv1.Dropped
}

func (q *wqueue) drops() *relayv1.Dropped {
	if q.dropped == nil {
		q.dropped = relayv1.Dropped_builder{}.Build()
	}
	return q.dropped
}

func newWatcher(agent string) *watcher {
	return &watcher{agent: agent, queues: map[string]*wqueue{}, wake: make(chan struct{}, 1)}
}

func (w *watcher) wants(agent string) bool { return w.agent == "" || w.agent == agent }

func (w *watcher) queue(agent string) *wqueue {
	q, ok := w.queues[agent]
	if !ok {
		q = &wqueue{}
		w.queues[agent] = q
		w.order = append(w.order, agent)
	}
	return q
}

// class is how a watcher queues an event.
type class int

const (
	// turnOutput may be dropped when the watch falls behind.
	turnOutput class = iota
	// turnEnd, a task's last event, queues after its output and is never
	// dropped.
	turnEnd
	// note is a question, a message or a Dropped: ahead of turn output.
	note
)

// push queues ev.
func (w *watcher) push(ev *relayv1.WatchEvent, c class, msg uint64) {
	it := witem{ev: ev, msg: msg, size: proto.Size(ev)}
	w.mu.Lock()
	q := w.queue(ev.GetAgent())
	switch {
	case c == note && q.noteBytes+it.size > watchNoteBytes:
		countDropped(q.drops(), ev, "the watch fell behind")
	case c == note:
		q.notes = append(q.notes, it)
		q.noteBytes += it.size
	case c == turnOutput && q.turnBytes+it.size > watchTurnBytes:
		q.drops().SetTurnEvents(q.drops().GetTurnEvents() + 1)
		q.drops().SetReason("the watch fell behind this agent's turn output")
	default:
		q.turn = append(q.turn, it)
		q.turnBytes += it.size
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// countDropped counts ev, dropped, in d.
func countDropped(d *relayv1.Dropped, ev *relayv1.WatchEvent, reason string) {
	switch ev.WhichBody() {
	case relayv1.WatchEvent_Question_case:
		d.SetQuestions(d.GetQuestions() + 1)
	case relayv1.WatchEvent_Message_case:
		d.SetMessages(d.GetMessages() + 1)
	case relayv1.WatchEvent_Dropped_case:
		d.SetTurnEvents(d.GetTurnEvents() + ev.GetDropped().GetTurnEvents())
		d.SetQuestions(d.GetQuestions() + ev.GetDropped().GetQuestions())
		d.SetMessages(d.GetMessages() + ev.GetDropped().GetMessages())
		d.SetInvalid(d.GetInvalid() + ev.GetDropped().GetInvalid())
	}
	d.SetReason(reason)
}

func dropsAny(d *relayv1.Dropped) bool {
	return d.GetTurnEvents()+d.GetQuestions()+d.GetMessages()+d.GetInvalid() > 0
}

// pop takes the next item, an agent's in turn: its Dropped marker first,
// then its notes, then its turn output.
func (w *watcher) pop() (witem, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.order {
		k := (w.next + i) % len(w.order)
		agent := w.order[k]
		q := w.queues[agent]
		var it witem
		switch {
		case dropsAny(q.dropped):
			d := q.dropped
			q.dropped = nil
			it = witem{ev: relayv1.WatchEvent_builder{Agent: agent, Dropped: d}.Build()}
		case len(q.notes) > 0:
			it, q.notes = q.notes[0], q.notes[1:]
			q.noteBytes -= it.size
		case len(q.turn) > 0:
			it, q.turn = q.turn[0], q.turn[1:]
			q.turnBytes -= it.size
		default:
			continue
		}
		w.next = k + 1
		return it, true
	}
	return witem{}, false
}

// run sends the watcher's events on st until ctx ends or a send fails;
// delivered is told of each message sent.
func (w *watcher) run(ctx context.Context, send func(*relayv1.WatchEvent) error, delivered func(agent string, n uint64)) error {
	for {
		it, ok := w.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return nil
			case <-w.wake:
			}
			continue
		}
		if err := send(it.ev); err != nil {
			return err
		}
		if it.msg != 0 {
			delivered(it.ev.GetAgent(), it.msg)
		}
	}
}
