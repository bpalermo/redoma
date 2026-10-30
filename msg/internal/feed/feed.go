// Package feed is redoma mcp's view of the relay's Watch: one
// long-lived Watch of every agent, redialled when it breaks, kept in a
// bounded buffer that the lead polls by cursor.
//
// The buffer keeps what the lead has not seen of what matters: a question
// or a message stays until a poll has returned it, and while more than
// Config.MaxHeld bytes of them wait the Watch is not read (the relay keeps
// them meanwhile; it never drops them for a slow watch). Turn output does
// not wait: past Config.MaxBytes or Config.MaxEvents the oldest goes, an
// unreturned one replaced by a marker that counts it, one per agent.
//
// The relay replays the pending questions to every new Watch; one this
// buffer has already taken is not taken again.
package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
)

// Config bounds the buffer and paces the redials.
type Config struct {
	// MaxBytes and MaxEvents bound the buffer; turn output goes first.
	MaxBytes  int
	MaxEvents int
	// MaxHeld bounds the unreturned questions' and messages' bytes; past it
	// the Watch waits for a poll.
	MaxHeld int
	// MinBackoff and MaxBackoff pace the redials of a broken Watch.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

// DefaultConfig is what redoma mcp runs with.
var DefaultConfig = Config{
	MaxBytes:   32 << 20,
	MaxEvents:  50000,
	MaxHeld:    8 << 20,
	MinBackoff: 500 * time.Millisecond,
	MaxBackoff: 15 * time.Second,
}

// maxSeen bounds the question ids remembered against the replays.
const maxSeen = 4096

// overhead is what an event is counted besides its texts.
const overhead = 64

// Event is one entry of the buffer: a WatchEvent, or (Ev nil) a marker of
// the turn output of Agent that the buffer dropped, Lost events of it.
type Event struct {
	Cursor uint64
	Ev     *relayv1.WatchEvent
	Agent  string
	Lost   uint64
	// Cont: the event's chunk goes on a line the task's previous event
	// began (that one was partial).
	Cont bool

	returned bool
	size     int
}

// held reports whether the event is one kept until returned.
func (e *Event) held() bool {
	return e.Ev.GetQuestion() != nil || e.Ev.GetMessage() != nil
}

// Feed is the buffer and the Watch that fills it.
type Feed struct {
	lead relayv1.LeadClient
	cfg  Config

	mu       sync.Mutex
	events   []*Event
	last     uint64
	bytes    int
	heldSize int
	lost     map[string]*Event
	partial  map[string]bool
	seen     map[string]bool
	seenFIFO []string
	// added is closed and replaced when an event is added, space when a
	// held event is returned.
	added chan struct{}
	space chan struct{}
	// connected and err: the Watch's state, for the polls to show.
	connected bool
	err       error
	// paused: the Watch waits for a poll to return held events.
	paused bool
}

// New is a Feed of the relay's Lead service; Run fills it.
func New(lead relayv1.LeadClient, cfg Config) *Feed {
	return &Feed{
		lead: lead, cfg: cfg,
		lost: map[string]*Event{}, partial: map[string]bool{}, seen: map[string]bool{},
		added: make(chan struct{}), space: make(chan struct{}),
	}
}

// Run watches every agent until ctx ends, redialling a Watch that broke.
func (f *Feed) Run(ctx context.Context) {
	backoff := f.cfg.MinBackoff
	for ctx.Err() == nil {
		got, err := f.watch(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil || errors.Is(err, io.EOF) {
			err = errors.New("the relay ended the watch")
		}
		f.mu.Lock()
		f.connected, f.err = false, err
		f.mu.Unlock()
		if got {
			backoff = f.cfg.MinBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, f.cfg.MaxBackoff)
	}
}

// watch runs one Watch until it breaks; got reports whether it delivered
// anything.
func (f *Feed) watch(ctx context.Context) (got bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := f.lead.Watch(ctx, relayv1.WatchRequest_builder{}.Build())
	if err != nil {
		return false, err
	}
	for {
		ev, err := st.Recv()
		if err != nil {
			return got, err
		}
		if !got {
			got = true
			f.mu.Lock()
			f.connected, f.err = true, nil
			f.mu.Unlock()
		}
		if err := f.add(ctx, ev); err != nil {
			return got, err
		}
	}
}

// State is the Watch's: "connected", or why it is being redialled.
func (f *Feed) State() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.connected && f.paused:
		return fmt.Sprintf("paused: %d bytes of questions and messages no poll has returned (a poll of every agent takes them)", f.heldSize)
	case f.connected:
		return "connected"
	case f.err != nil:
		return "reconnecting: " + f.err.Error()
	default:
		return "connecting"
	}
}

// add buffers one event, waiting while the held events are over MaxHeld.
func (f *Feed) add(ctx context.Context, ev *relayv1.WatchEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := &Event{Ev: ev, Agent: ev.GetAgent(), size: size(ev)}
	if q := ev.GetQuestion(); q != nil {
		if f.seen[q.GetId()] {
			return nil
		}
		f.remember(q.GetId())
	}
	if te := ev.GetTurnEvent(); te != nil {
		task := te.GetTaskId()
		e.Cont = f.partial[task]
		if te.GetPartial() {
			f.partial[task] = true
		} else {
			delete(f.partial, task)
		}
	}
	if e.held() {
		for f.heldSize > 0 && f.heldSize+e.size > f.cfg.MaxHeld {
			space := f.space
			f.paused = true
			f.mu.Unlock()
			select {
			case <-ctx.Done():
				f.mu.Lock()
				f.paused = false
				return ctx.Err()
			case <-space:
			}
			f.mu.Lock()
		}
		f.paused = false
		f.heldSize += e.size
	}
	f.last++
	e.Cursor = f.last
	f.events = append(f.events, e)
	f.bytes += e.size
	f.evict()
	close(f.added)
	f.added = make(chan struct{})
	return nil
}

// remember keeps a question id against the replays, the oldest forgotten
// past maxSeen.
func (f *Feed) remember(id string) {
	f.seen[id] = true
	f.seenFIFO = append(f.seenFIFO, id)
	if len(f.seenFIFO) > maxSeen {
		delete(f.seen, f.seenFIFO[0])
		f.seenFIFO = f.seenFIFO[1:]
	}
}

// evict drops the oldest events it may until the buffer is in its bounds:
// any the lead was given, and unreturned turn output, which a marker
// counts.
func (f *Feed) evict() {
	for f.bytes > f.cfg.MaxBytes || len(f.events) > f.cfg.MaxEvents {
		i := slices.IndexFunc(f.events, func(e *Event) bool { return e.returned || e.Ev.GetTurnEvent() != nil })
		if i < 0 {
			return
		}
		f.drop(i)
	}
}

// drop drops the i-th event; unreturned turn output is counted by its
// agent's marker, which takes its place when there is none yet.
func (f *Feed) drop(i int) {
	e := f.events[i]
	f.bytes -= e.size
	if !e.returned && e.Ev.GetTurnEvent() != nil {
		if m := f.lost[e.Agent]; m != nil {
			m.Lost++
		} else {
			m = &Event{Cursor: e.Cursor, Agent: e.Agent, Lost: 1, size: overhead}
			f.lost[e.Agent] = m
			f.events[i] = m
			f.bytes += m.size
			return
		}
	}
	f.events = slices.Delete(f.events, i, i+1)
}

// size is what an event is counted.
func size(ev *relayv1.WatchEvent) int {
	n := overhead + len(ev.GetAgent())
	if te := ev.GetTurnEvent(); te != nil {
		n += len(te.GetTaskId()) + len(te.GetChunk()) + len(te.GetError())
	}
	if q := ev.GetQuestion(); q != nil {
		n += len(q.GetId()) + len(q.GetTaskId()) + len(q.GetText())
	}
	if m := ev.GetMessage(); m != nil {
		n += len(m.GetTaskId()) + len(m.GetText())
	}
	return n
}

// Take is called on each event a poll finds, in order; it returns false
// when the poll's response is full, and the event is left for the next.
type Take func(*Event) bool

// Poll offers take the events after pos (of agent only, unless it is
// empty), waiting up to wait for one when there is none. It returns the
// position to poll from next, and whether take refused one (there is
// more). The events take accepted are returned: a question or a message
// may then be dropped. Events of other agents are passed over, and are
// not (Pos).
func (f *Feed) Poll(ctx context.Context, agent string, pos Pos, wait time.Duration, take Take) (next Pos, more bool) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		f.mu.Lock()
		next, more, any := f.scan(agent, pos, take)
		added := f.added
		f.mu.Unlock()
		if any || more || wait <= 0 {
			return next, more
		}
		pos = next
		select {
		case <-added:
		case <-deadline.C:
			return next, false
		case <-ctx.Done():
			return next, false
		}
	}
}

// Pos is where a poll is: N, the last event it went past, and Low, the
// last up to which every event was returned (by this poll or another). A
// poll of all agents has both equal. A poll of one agent passes over the
// others' events, and a poll from its Pos, of whatever agents, starts
// after Low but offers of the events up to N only those no poll returned:
// what the first passed over is still seen, never lost, and what it
// returned does not come again. Low moves on as soon as what was passed
// over has been returned.
type Pos struct {
	N, Low uint64
}

// scan offers take the events after pos.N; any reports whether it took
// one. Called with mu held.
func (f *Feed) scan(agent string, pos Pos, take Take) (next Pos, more, any bool) {
	if pos.N > f.last || pos.Low > pos.N {
		// A cursor of another process's: from the start.
		pos = Pos{}
	}
	start := f.first() - 1
	next = Pos{N: max(pos.N, start), Low: max(pos.Low, start)}
	low, past := lowMark{at: next.Low}, next.N
	for _, e := range f.events {
		switch {
		case e.Cursor <= next.Low:
		case e.Cursor <= past && e.returned:
			// Went past by this cursor, and returned: not again.
			low.see(e)
		case agent != "" && e.Agent != agent:
			low.see(e)
			next.N = max(next.N, e.Cursor)
		case !take(e):
			// Not returned: Low stops before it, or a later poll would
			// start past it.
			low.see(e)
			next.Low = low.result(next.N)
			return next, true, any
		default:
			any = true
			next.N = max(next.N, e.Cursor)
			f.returned(e)
			low.see(e)
		}
	}
	next.Low = low.result(next.N)
	return next, false, any
}

// lowMark finds a Pos's Low: it follows the events in order while each is
// returned (one gone from the buffer was, or is counted by a marker) and
// stops at the first that is not.
type lowMark struct {
	at      uint64
	stopped bool
}

func (l *lowMark) see(e *Event) {
	switch {
	case l.stopped:
	case !e.returned:
		l.stopped = true
	default:
		l.at = e.Cursor
	}
}

// result is Low for a poll that went past n.
func (l *lowMark) result(n uint64) uint64 {
	if l.stopped {
		return l.at
	}
	return n
}

// first is the oldest cursor buffered, or the next one when none is.
func (f *Feed) first() uint64 {
	if len(f.events) == 0 {
		return f.last + 1
	}
	return f.events[0].Cursor
}

// returned marks an event a poll returned.
func (f *Feed) returned(e *Event) {
	if e.returned {
		return
	}
	e.returned = true
	if e.Ev == nil && f.lost[e.Agent] == e {
		delete(f.lost, e.Agent)
	}
	if e.Ev != nil && e.held() {
		f.heldSize -= e.size
		close(f.space)
		f.space = make(chan struct{})
	}
}
