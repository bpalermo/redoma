// Package agent is what the control plane knows of a running agent, and the
// interface of whatever tells it which agents run.
//
// The control plane never derives a namespace path or a selector from an
// agent's name: both come from the Source: activedir, a launcher's state in
// /run/redoma, or hooksource, the OCI hook's registrations, which carry a
// container's own namespace.
package agent

import (
	"context"
	"slices"
)

// Selector is a SPIRE selector, type:value.
type Selector struct {
	Type  string
	Value string
}

// Agent is one running agent.
type Agent struct {
	// ID names the agent: its repo. Its listeners, its secret and its SPIFFE
	// ID (spiffe://<trust domain>/agent/<ID>) are named after it.
	ID string
	// Netns is the path of the agent's network namespace, where its
	// listeners are bound.
	Netns string
	// NetnsID is which namespace is at Netns. A namespace replaced at the
	// same path is a different agent to Envoy: its listeners must be bound
	// again, in the new namespace.
	NetnsID NetnsID
	// Selectors are what its identity is subscribed by. SPIRE takes them as
	// attested, so they decide which identity the agent gets.
	Selectors []Selector
}

// NetnsID identifies a namespace: the device and inode of its nsfs file
// (what stat(2) of the bind mount, or of /proc/<pid>/ns/net, reports).
type NetnsID struct {
	Dev uint64
	Ino uint64
}

// Equal reports whether a and b are the same agent in the same namespace
// with the same selectors.
func (a Agent) Equal(b Agent) bool {
	return a.ID == b.ID && a.Netns == b.Netns && a.NetnsID == b.NetnsID && slices.Equal(a.Selectors, b.Selectors)
}

// Event is an agent starting or stopping, or the end of the initial set.
type Event struct {
	Agent Agent
	// Removed is true when the agent stopped. A start of an ID that is
	// already running replaces it (a new namespace, the same path with
	// another namespace at it, new selectors).
	Removed bool
	// Synced marks that every agent running when the Source started has
	// been emitted; Agent and Removed are unset. Sent once. Until then the
	// control plane publishes nothing: a snapshot of a partial set would
	// make a connected Envoy drop the listeners of the agents not in it yet.
	Synced bool
	// Bound, when not nil on a start, receives one value: nil once Envoy
	// has reported a snapshot with the agent's listeners applied (so they
	// are bound in its namespace), or an error if the agent is stopped or
	// replaced first, or the control plane stops. It must have room for one
	// value: the control plane never blocks on it.
	Bound chan<- error
}

// Source says which agents run.
type Source interface {
	// Run calls emit for every change until ctx ends, starting with an Added
	// event for each agent running when it starts, then a Synced event.
	// emit is called from one
	// goroutine at a time. Run returns nil when ctx ends, or an error that
	// stops the control plane.
	Run(ctx context.Context, emit func(Event)) error
}
