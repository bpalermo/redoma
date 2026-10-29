package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
)

// Merge runs several Sources as one. They are in priority order: when more
// than one claims an agent ID, the earliest Source's claim is the one
// passed on, whatever the order the claims arrive in. A later Source's
// start that waits for its Bound (a registrar holding a container's start)
// is refused at once while an earlier Source holds the ID, so the container
// does not start without its proxy. Any other claim is kept, and offered
// again when every earlier claim of the ID is gone; an earlier Source's
// claim replaces a later one's that was passed on. So two registrars never
// bind one identity twice, and neither is starved for good. Synced is sent
// once every Source has sent it. The first Source to return ends them all,
// with its error.
func Merge(log *slog.Logger, sources ...Source) Source {
	return &merged{log: log, sources: sources}
}

type merged struct {
	log     *slog.Logger
	sources []Source
}

// mergeState is one Run's bookkeeping.
type mergeState struct {
	log  *slog.Logger
	emit func(Event)
	mu   sync.Mutex
	// claims holds, per agent ID, each Source's latest start, Bound
	// removed once it has been passed on or refused.
	claims map[string]map[int]Event
	// passed is, per agent ID, the Source whose claim was passed on last.
	passed map[string]int
	synced []bool
	sent   bool
}

func (m *merged) Run(ctx context.Context, emit func(Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st := &mergeState{
		log:    m.log,
		emit:   emit,
		claims: map[string]map[int]Event{},
		passed: map[string]int{},
		synced: make([]bool, len(m.sources)),
	}
	errc := make(chan error, len(m.sources))
	for i, s := range m.sources {
		go func() { errc <- s.Run(ctx, func(e Event) { st.forward(i, e) }) }()
	}
	err := <-errc
	if err == nil && ctx.Err() == nil {
		err = errors.New("an agent source stopped")
	}
	cancel()
	for range len(m.sources) - 1 {
		<-errc
	}
	return err
}

// forward records Source i's event and passes on what changes.
func (st *mergeState) forward(i int, e Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if e.Synced {
		st.synced[i] = true
		if !st.sent && !slices.Contains(st.synced, false) {
			st.sent = true
			st.emit(e)
		}
		return
	}
	id := e.Agent.ID
	if !e.Removed && e.Bound != nil && st.heldBefore(id, i) {
		st.log.Error("agent already registered by an earlier source; refusing", "agent", id, "netns", e.Agent.Netns)
		e.Bound <- fmt.Errorf("agent %q is already registered by another registrar", id)
		return
	}
	if st.claims[id] == nil {
		st.claims[id] = map[int]Event{}
	}
	if e.Removed {
		delete(st.claims[id], i)
	} else {
		st.claims[id][i] = e
	}
	st.pass(id, i)
}

// heldBefore reports whether a Source earlier than i claims id.
func (st *mergeState) heldBefore(id string, i int) bool {
	for j := range st.claims[id] {
		if j < i {
			return true
		}
	}
	return false
}

// pass passes on id's winning claim, if it is not what was passed last
// (or is Source i's new start of it), or the removal when none is left.
func (st *mergeState) pass(id string, i int) {
	claims := st.claims[id]
	last, passed := st.passed[id]
	if len(claims) == 0 {
		delete(st.claims, id)
		delete(st.passed, id)
		if passed {
			st.emit(Event{Agent: Agent{ID: id}, Removed: true})
		}
		return
	}
	best := -1
	for j := range claims {
		if best == -1 || j < best {
			best = j
		}
	}
	if passed && best == last && best != i {
		return // the change was a later Source's claim, kept for later
	}
	if passed && best != last {
		st.log.Warn("agent passes to another source's claim", "agent", id, "from", last, "to", best)
	}
	e := claims[best]
	st.emit(e)
	e.Bound = nil // answered by the control plane; never again on a replay
	claims[best] = e
	st.passed[id] = best
}
