// Package hub is redoma-relay's state and its two gRPC services: Relay, the
// agents' one stream each, and Lead, the lead's local API.
//
// Per agent, under <Dir>/agents/<name>/: the queue of envelopes to it not yet
// acked (out/, reliable.Outbox, synced on every push: tasks, replies and
// cancels are few and must survive a power loss), the record of what came
// from it (in.json, reliable.Inbox), and the journal of what the lead
// depends on (lead.log, store.go): the tasks open with it, its questions not
// yet answered, its messages not yet delivered.
//
// The agent side is untrusted (limits.go): every envelope from an agent is
// checked against that agent's own state, bounded in size, count and rate,
// and dropped, with a Dropped event to the lead, when it is not valid.
// Routing is lead <-> agent only: an agent's Message is for the lead or for
// nobody, and no task, question or message of one agent is another's.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/common/allowlist"
	"github.com/bpalermo/redoma/common/reliable"
	"github.com/bpalermo/redoma/relay/internal/mtls"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Hub holds every agent's state. Make it with New.
type Hub struct {
	dir         string
	trustDomain string
	allowed     mtls.Allowed
	ackEvery    time.Duration
	log         *slog.Logger
	// clock is time.Now unless a test sets one.
	clock atomic.Pointer[func() time.Time]

	// wg counts the calls and the agents' receiving goroutines in flight,
	// which Close waits for.
	wg sync.WaitGroup
	// held is the bytes of messages held, across agents.
	held atomic.Int64

	mu       sync.Mutex
	agents   map[string]*agentState
	watchers map[*watcher]struct{}
	// quarantined are the agents whose state could not be opened, and
	// why: refused (their streams and the lead's calls for them) until a
	// later open succeeds, the others served meanwhile.
	quarantined map[string]error
	// openMu makes opens one at a time (agent).
	openMu sync.Mutex
}

// Config is what New needs.
type Config struct {
	// Dir is the state directory (/var/lib/redoma-relay).
	Dir string
	// TrustDomain is the agents' SPIFFE trust domain.
	TrustDomain string
	// Allowed says which agent names exist (the allowlist).
	Allowed mtls.Allowed
	// AckEvery is how often what an agent sent is saved and acked.
	AckEvery time.Duration
	Log      *slog.Logger
}

// New opens the state of every agent found under cfg.Dir, so that their
// pending questions and held messages are there for the first Watch.
func New(cfg Config) (*Hub, error) {
	h := &Hub{
		dir:         cfg.Dir,
		trustDomain: cfg.TrustDomain,
		allowed:     cfg.Allowed,
		ackEvery:    cfg.AckEvery,
		log:         cfg.Log,
		agents:      map[string]*agentState{},
		watchers:    map[*watcher]struct{}{},
		quarantined: map[string]error{},
	}
	entries, err := os.ReadDir(filepath.Join(cfg.Dir, "agents"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !allowlist.ValidName(e.Name()) {
			continue
		}
		// An agent whose state cannot be opened is quarantined, not the
		// relay stopped: the others go on being served (h.agent).
		_, _ = h.agent(e.Name())
	}
	return h, nil
}

// Close closes every agent's files, once the calls in flight have ended:
// stop the gRPC servers first.
func (h *Hub) Close() {
	h.wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.agents {
		a.out.Close()
		a.mu.Lock()
		a.st.close()
		a.mu.Unlock()
	}
}

func (h *Hub) now() time.Time {
	if f := h.clock.Load(); f != nil {
		return (*f)()
	}
	return time.Now()
}

// agentState is one agent's.
type agentState struct {
	name string
	dir  string
	out  *reliable.Outbox
	in   *reliable.Inbox
	log  *slog.Logger

	mu  sync.Mutex
	st  *store
	cur *attachment
	// broken tells the lead the journal broke.
	broken func(error)
	// The agent's rates (limits.go), and its inbound drops not yet told.
	turnBytes, turnEvents, notes bucket
	drops                        *relayv1.Dropped
	lastMarker                   time.Time
}

// agent returns the agent's state, opened (and made) on first use. One
// that cannot be opened (a corrupt journal, a record that does not parse)
// quarantines the agent: the error is logged and kept, and each later use
// tries again, so a repaired state is taken back without a restart.
//
// The open runs under openMu, not h.mu: opening reads and compacts files,
// and the other agents' calls go on meanwhile; opens are one at a time,
// so two never compact one journal at once.
func (h *Hub) agent(name string) (*agentState, error) {
	if a, ok := h.known(name); ok {
		return a, nil
	}
	h.openMu.Lock()
	defer h.openMu.Unlock()
	if a, ok := h.known(name); ok {
		return a, nil
	}
	a, err := h.open(name)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		if _, ok := h.quarantined[name]; !ok {
			h.log.Error("an agent's state could not be opened; the agent is quarantined, the others served", "agent", name, "err", err)
		}
		h.quarantined[name] = err
		return nil, err
	}
	if _, ok := h.quarantined[name]; ok {
		h.log.Info("an agent's state opens again; the agent is served", "agent", name)
	}
	delete(h.quarantined, name)
	h.agents[name] = a
	return a, nil
}

// isQuarantined is why the agent is quarantined, if it is.
func (h *Hub) isQuarantined(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.quarantined[name]
}

// open opens an agent's state; openMu is held.
func (h *Hub) open(name string) (*agentState, error) {
	dir := filepath.Join(h.dir, "agents", name)
	out, err := reliable.OpenOutbox(filepath.Join(dir, "out"), true)
	if err != nil {
		return nil, fmt.Errorf("agent %s's queue: %w", name, err)
	}
	in, err := reliable.OpenInbox(filepath.Join(dir, "in.json"))
	if err != nil {
		out.Close()
		return nil, fmt.Errorf("agent %s's record: %w", name, err)
	}
	st, err := openStore(filepath.Join(dir, "lead.log"))
	if err != nil {
		out.Close()
		return nil, fmt.Errorf("agent %s's journal: %w", name, err)
	}
	h.held.Add(int64(st.msgBytes))
	a := &agentState{
		name: name, dir: dir, out: out, in: in, st: st, log: h.log.With("agent", name),
		turnBytes:  newBucket(turnBytesPerSec, turnBytesBurst),
		turnEvents: newBucket(turnEventsPerSec, turnEventsBurst),
		notes:      newBucket(notesPerSec, notesBurst),
	}
	a.broken = func(err error) {
		h.broadcast(brokenEvent(name, err), note, 0)
	}
	return a, nil
}

// brokenEvent tells the lead an agent's state is unusable.
func brokenEvent(agent string, err error) *relayv1.WatchEvent {
	return relayv1.WatchEvent_builder{Agent: agent, Dropped: relayv1.Dropped_builder{
		Invalid: 1, Reason: text("the relay's state of this agent is broken, and the agent refused until it is repaired: "+err.Error(), maxError),
	}.Build()}.Build()
}

// known is the agent's state if it has any, without making it.
func (h *Hub) known(name string) (*agentState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[name]
	return a, ok
}

// matching are the agents a watch of agent ("" for all) sees, by name.
func (h *Hub) matching(agent string) []*agentState {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*agentState
	for _, name := range slices.Sorted(maps.Keys(h.agents)) {
		if agent == "" || agent == name {
			out = append(out, h.agents[name])
		}
	}
	return out
}

// record appends to the agent's journal; a.mu is held. The journal
// breaking is told to the lead once: from then on nothing more of the
// agent's is taken or acked.
func (a *agentState) record(r record) error {
	wasBroken := a.st.broken != nil
	err := a.st.add(r)
	if err != nil {
		a.log.Error("could not write the agent's journal", "err", err)
	}
	if a.st.compactErr != nil {
		a.log.Warn("could not compact the agent's journal; appending to it still", "err", a.st.compactErr)
		a.st.compactErr = nil
	}
	if !wasBroken && a.st.broken != nil {
		a.log.Error("the agent's journal is broken; the agent is refused until a restart reopens it", "err", a.st.broken)
		// Quarantined from now on: its stream ends, and nothing more of
		// it is taken (inbound) or sent (refused).
		if a.cur != nil {
			a.cur.cancel(errBroken)
		}
		go a.broken(a.st.broken)
	}
	return err
}

// errBroken ends the stream of an agent whose journal broke.
var errBroken = errors.New("the relay's journal of this agent broke")

// refused is the lead's error for an agent whose journal is broken, nil
// for one that is fine; a.mu is held.
func (a *agentState) refused() error {
	if a.st.broken == nil {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition, "the relay's state of agent %s is broken; the agent is quarantined: %s", a.name, text(a.st.broken.Error(), maxError))
}

func (a *agentState) journalBroken() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.broken != nil
}

// syncJournal makes the journal durable before an ack (Session.BeforeAck).
func (a *agentState) syncJournal() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.sync()
}

// errReplaced ends a stream that a newer one of the same agent replaced.
var errReplaced = errors.New("replaced by a newer stream of the same agent")

type attachment struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// attach makes this stream the agent's one stream: an older one is ended
// (errReplaced), and attach returns once it has stopped using the queue. The
// newer stream wins because a reconnect is what follows a connection that
// broke without either end noticing, and the older stream is then the dead
// one; the client certificate is the same agent's either way. release ends
// the attachment.
//
// A journal broken by then (between the caller's check and this, or while
// an older stream was let go) refuses the attachment: errBroken, under the
// same a.mu that record breaks it under, so no stream of a broken agent is
// ever attached.
func (a *agentState) attach(parent context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithCancelCause(parent)
	me := &attachment{cancel: cancel, done: make(chan struct{})}
	a.mu.Lock()
	for a.cur != nil {
		old := a.cur
		old.cancel(errReplaced)
		a.mu.Unlock()
		<-old.done
		a.mu.Lock()
	}
	if a.st.broken != nil {
		a.mu.Unlock()
		cancel(errBroken)
		return nil, nil, errBroken
	}
	a.cur = me
	a.mu.Unlock()
	return ctx, func() {
		a.mu.Lock()
		if a.cur == me {
			a.cur = nil
		}
		a.mu.Unlock()
		close(me.done)
		cancel(nil)
	}, nil
}

func questionEvent(agent string, q held) *relayv1.WatchEvent {
	return relayv1.WatchEvent_builder{Agent: agent, Question: relayv1.Question_builder{
		Id: agent + "/" + q.ID, Text: q.Text, TaskId: q.TaskID,
	}.Build()}.Build()
}

func messageEvent(agent string, m held) *relayv1.WatchEvent {
	return relayv1.WatchEvent_builder{Agent: agent, Message: relayv1.Message_builder{
		To: "lead", Text: m.Text, TaskId: m.TaskID,
	}.Build()}.Build()
}

func (h *Hub) watch(agent string) *watcher {
	w := newWatcher(agent)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watchers[w] = struct{}{}
	return w
}

func (h *Hub) unwatch(w *watcher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.watchers, w)
}

// broadcast queues ev on every watcher of its agent; msg is a message's
// number, 0 for anything else.
func (h *Hub) broadcast(ev *relayv1.WatchEvent, c class, msg uint64) {
	h.mu.Lock()
	ws := slices.Collect(maps.Keys(h.watchers))
	h.mu.Unlock()
	for _, w := range ws {
		if w.wants(ev.GetAgent()) {
			w.push(ev, c, msg)
		}
	}
}

// replay queues on w what it is owed first: every pending question and
// every held message of the agents it watches.
func (h *Hub) replay(w *watcher) {
	h.mu.Lock()
	quarantined := maps.Clone(h.quarantined)
	h.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(quarantined)) {
		if w.wants(name) {
			w.push(brokenEvent(name, quarantined[name]), note, 0)
		}
	}
	for _, a := range h.matching(w.agent) {
		a.mu.Lock()
		broken := a.st.broken
		a.mu.Unlock()
		if broken != nil {
			w.push(brokenEvent(a.name, broken), note, 0)
		}
		a.mu.Lock()
		qs, ms := slices.Clone(a.st.questions), slices.Clone(a.st.messages)
		a.mu.Unlock()
		for _, q := range qs {
			w.push(questionEvent(a.name, q), note, 0)
		}
		for _, m := range ms {
			w.push(messageEvent(a.name, m), note, m.N)
		}
	}
}

// delivered drops the agent's held message n: a watch has sent it.
func (h *Hub) delivered(agent string, n uint64) {
	a, ok := h.known(agent)
	if !ok {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.st.messages, func(m held) bool { return m.N == n })
	if i < 0 {
		return
	}
	size := heldSize(a.st.messages[i].Text)
	if a.record(record{Op: "delivered", held: held{N: n}}) == nil {
		h.held.Add(-int64(size))
	}
}

// newID is a random id, unique enough for a task.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// splitID splits a lead-side id, "<agent>/<id>".
func splitID(id string) (string, string, bool) {
	agent, rest, ok := strings.Cut(id, "/")
	return agent, rest, ok && allowlist.ValidName(agent) && rest != ""
}
