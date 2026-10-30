// Package leadmcp is redoma mcp: the Lead API as MCP tools, for a
// lead that is a Claude Code session (whose Bash sandbox cannot open
// unix sockets; an MCP server runs as the user, outside it).
//
//   - send(agent, text): queue a task; returns its id;
//   - reply(question, text): answer a question (<agent>/<id>);
//   - cancel(task): cancel a task (<agent>/<id>);
//   - poll(agent?, cursor?, wait_seconds?, max_bytes?, raw?): what the agents
//     sent since the cursor, from the server's one Watch (package feed),
//     and the cursor to poll from next.
//
// Inputs are bounded as the relay bounds them, and checked here first so
// that a bad one fails with what is wrong.
package leadmcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
	"github.com/bpalermo/redoma/common/allowlist"
	"github.com/bpalermo/redoma/common/mcpserver"
	"github.com/bpalermo/redoma/msg/internal/feed"
	"github.com/bpalermo/redoma/msg/internal/lines"
)

const (
	// MaxText is the relay's bound on a task's or a reply's text.
	MaxText = 1 << 20
	// MaxWait caps a poll's wait, under the MCP client's call timeout.
	MaxWait = 50 * time.Second
	// DefaultMaxBytes, MinMaxBytes and MaxMaxBytes bound a poll's response.
	DefaultMaxBytes = 64 << 10
	MinMaxBytes     = 4 << 10
	MaxMaxBytes     = 256 << 10
	// envelope is what a response takes besides its events.
	envelope = 1 << 10
)

// id is the part after the agent of a question's or a task's id: the
// shim's question ids, which the relay's task ids also are.
var id = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Server is one redoma mcp session.
type Server struct {
	Lead    relayv1.LeadClient
	Feed    *feed.Feed
	Version string

	// epoch tells this process's cursors from another's.
	epoch string
}

// New is a Server of lead, whose Watch f follows.
func New(lead relayv1.LeadClient, f *feed.Feed, version string) *Server {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return &Server{Lead: lead, Feed: f, Version: version, epoch: hex.EncodeToString(b[:])}
}

// MCP is the session's MCP server.
func (s *Server) MCP() *mcpserver.Server {
	return &mcpserver.Server{Name: "redoma-relay", Version: s.Version, Tools: []mcpserver.Tool{
		{
			Name:        "send",
			Description: "Queue a task for an agent; it runs once the agent is free. Returns the task's id (<agent>/<id>). Its output, questions and messages come through poll.",
			InputSchema: object(map[string]any{
				"agent": str("the agent's name (its repo)"),
				"text":  str("the task, complete enough to be done without other context"),
			}, "agent", "text"),
			Call: s.send,
		},
		{
			Name:        "reply",
			Description: "Answer an agent's question; its turn waits for the answer. The question's id (<agent>/<id>) is what poll showed.",
			InputSchema: object(map[string]any{
				"question": str("the question's id, <agent>/<id>"),
				"text":     str("the answer"),
			}, "question", "text"),
			Call: s.reply,
		},
		{
			Name:        "cancel",
			Description: "Cancel an agent's task, running or queued, by its id (<agent>/<id>).",
			InputSchema: object(map[string]any{"task": str("the task's id, <agent>/<id>")}, "task"),
			Call:        s.cancel,
		},
		{
			Name: "poll",
			Description: "What the agents sent since cursor: their text, the tools they used, each turn's result and end, " +
				"and their questions (answer with reply) and messages in full; raw gives the stream-json events whole. " +
				"Returns JSON {cursor, events, more, omitted, watch}: poll again from cursor; more means the response " +
				"was full; omitted counts the events compact output left out. Without a cursor it starts at the oldest " +
				"event buffered. With wait_seconds it waits up to that long for an event when there is none. " +
				"A cursor of a poll of one agent, used for all agents (or another), gets what that poll passed over " +
				"and no poll returned since: nothing is lost, and nothing returned comes again.",
			InputSchema: object(map[string]any{
				"agent":        str("only this agent's events"),
				"cursor":       str("the cursor a previous poll returned"),
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": int(MaxWait / time.Second), "description": "how long to wait for an event when there is none (default 0)"},
				"max_bytes":    map[string]any{"type": "integer", "minimum": MinMaxBytes, "maximum": MaxMaxBytes, "description": fmt.Sprintf("the response's size bound (default %d); a question or a message alone may exceed it, never cut", DefaultMaxBytes)},
				"raw":          map[string]any{"type": "boolean", "description": "the events whole, as redoma msg watch prints them"},
			}),
			Call: s.poll,
		},
	}}
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func object(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

// decode reads a call's arguments, refusing any it does not know.
func decode(args json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

func checkAgent(name string) error {
	if !allowlist.ValidName(name) {
		return fmt.Errorf("not an agent name: %q", name)
	}
	return nil
}

func checkText(text string) error {
	if text == "" {
		return errors.New("a text is required")
	}
	if len(text) > MaxText {
		return fmt.Errorf("a text of %d bytes is over %d", len(text), MaxText)
	}
	return nil
}

// checkID checks a lead-side id, <agent>/<id>.
func checkID(kind, s string) error {
	agent, rest, ok := strings.Cut(s, "/")
	if !ok || !allowlist.ValidName(agent) || !id.MatchString(rest) {
		return fmt.Errorf("not a %s id (<agent>/<id>): %q", kind, s)
	}
	return nil
}

func (s *Server) send(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct{ Agent, Text string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if err := checkAgent(a.Agent); err != nil {
		return "", err
	}
	if err := checkText(a.Text); err != nil {
		return "", err
	}
	r, err := s.Lead.Send(ctx, relayv1.SendRequest_builder{Agent: a.Agent, Text: a.Text}.Build())
	if err != nil {
		return "", err
	}
	return r.GetTaskId(), nil
}

func (s *Server) reply(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct{ Question, Text string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if err := checkID("question", a.Question); err != nil {
		return "", err
	}
	if err := checkText(a.Text); err != nil {
		return "", err
	}
	if _, err := s.Lead.Reply(ctx, relayv1.ReplyRequest_builder{QuestionId: a.Question, Text: a.Text}.Build()); err != nil {
		return "", err
	}
	return "answered", nil
}

func (s *Server) cancel(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct{ Task string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if err := checkID("task", a.Task); err != nil {
		return "", err
	}
	if _, err := s.Lead.Cancel(ctx, relayv1.CancelRequest_builder{TaskId: a.Task}.Build()); err != nil {
		return "", err
	}
	return "cancelled", nil
}

// response is a poll's result.
type response struct {
	Cursor  string            `json:"cursor"`
	Events  []json.RawMessage `json:"events"`
	More    bool              `json:"more,omitempty"`
	Omitted int               `json:"omitted,omitempty"`
	// Reset: the cursor was another process's (redoma mcp restarted):
	// the events start at the oldest buffered.
	Reset bool   `json:"reset,omitempty"`
	Watch string `json:"watch"`
}

// pollArgs are poll's arguments, checked.
type pollArgs struct {
	agent    string
	pos      feed.Pos
	reset    bool
	wait     time.Duration
	maxBytes int
	raw      bool
}

func (s *Server) pollArgs(args json.RawMessage) (pollArgs, error) {
	var a struct {
		Agent       string
		Cursor      string
		WaitSeconds *int `json:"wait_seconds"`
		MaxBytes    *int `json:"max_bytes"`
		Raw         bool
	}
	if err := decode(args, &a); err != nil {
		return pollArgs{}, err
	}
	if a.Agent != "" {
		if err := checkAgent(a.Agent); err != nil {
			return pollArgs{}, err
		}
	}
	pos, reset, err := s.parseCursor(a.Cursor)
	if err != nil {
		return pollArgs{}, err
	}
	p := pollArgs{agent: a.Agent, pos: pos, reset: reset, maxBytes: DefaultMaxBytes, raw: a.Raw}
	if a.WaitSeconds != nil {
		if *a.WaitSeconds < 0 {
			return pollArgs{}, errors.New("wait_seconds must not be negative")
		}
		p.wait = min(time.Duration(*a.WaitSeconds)*time.Second, MaxWait)
	}
	if a.MaxBytes != nil {
		p.maxBytes = min(max(*a.MaxBytes, MinMaxBytes), MaxMaxBytes)
	}
	return p, nil
}

// page fills a response up to its budget.
type page struct {
	res    *response
	raw    bool
	budget int
	used   int
}

// take is feed.Take: it adds an event while it fits, the first one in any
// case, so that a poll always moves on: cut to fit, unless it is a
// question or a message, which goes whole (at most 16 KiB of text, as the
// relay bounds them) even past the bound, since once returned it may go.
func (p *page) take(e *feed.Event) bool {
	l, ok := render(e, p.raw)
	if !ok {
		p.res.Omitted++
		return true
	}
	b := marshal(l)
	if p.used+len(b)+1 > p.budget {
		if len(p.res.Events) > 0 {
			return false
		}
		held := e.Ev.GetQuestion() != nil || e.Ev.GetMessage() != nil
		for !held && len(b) > p.budget && l.Truncate(len(b)-p.budget) {
			b = marshal(l)
		}
	}
	p.res.Events = append(p.res.Events, b)
	p.used += len(b) + 1
	return true
}

func (s *Server) poll(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := s.pollArgs(args)
	if err != nil {
		return "", err
	}
	res := response{Events: []json.RawMessage{}, Reset: a.reset}
	p := &page{res: &res, raw: a.raw, budget: a.maxBytes - envelope}
	next, more := s.Feed.Poll(ctx, a.agent, a.pos, a.wait, p.take)
	res.Cursor, res.More = s.cursor(next), more
	res.Watch = s.Feed.State()
	if len(res.Watch) > 512 {
		res.Watch = strings.ToValidUTF8(res.Watch[:512], "")
	}
	return string(marshal(res)), nil
}

// cursor is the cursor of a poll's position (feed.Pos): <epoch>.<n>, or
// <epoch>.<n>.<low> when the poll passed over events no poll has returned.
func (s *Server) cursor(p feed.Pos) string {
	if p.Low == p.N {
		return s.epoch + "." + strconv.FormatUint(p.N, 10)
	}
	return fmt.Sprintf("%s.%d.%d", s.epoch, p.N, p.Low)
}

// maxCursor bounds a cursor: an epoch and two numbers (and an agent name,
// in an older format, which a restart resets).
const maxCursor = 128

// parseCursor reads a cursor, for a poll of any agent or all: feed.Pos
// offers what its poll passed over and no poll returned since. Another
// process's is reset to the start.
func (s *Server) parseCursor(c string) (feed.Pos, bool, error) {
	if c == "" {
		return feed.Pos{}, false, nil
	}
	bad := fmt.Errorf("not a cursor: %q", c)
	parts := strings.Split(c, ".")
	if len(c) > maxCursor || len(parts) < 2 {
		return feed.Pos{}, false, bad
	}
	if parts[0] != s.epoch {
		// Another process's, of whatever format it had (an older one
		// named the agent): from the start.
		return feed.Pos{}, true, nil
	}
	if len(parts) > 3 {
		return feed.Pos{}, false, bad
	}
	n, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return feed.Pos{}, false, bad
	}
	pos := feed.Pos{N: n, Low: n}
	if len(parts) == 3 {
		if pos.Low, err = strconv.ParseUint(parts[2], 10, 64); err != nil || pos.Low > n {
			return feed.Pos{}, false, bad
		}
	}
	return pos, false, nil
}

// render is an event as a poll shows it, and false for one compact output
// leaves out.
func render(e *feed.Event, raw bool) (lines.Line, bool) {
	if e.Ev == nil {
		return lines.Line{Agent: e.Agent, Dropped: &lines.Drop{
			TurnEvents: e.Lost,
			Reason:     "redoma mcp's buffer was full before a poll took them",
		}}, true
	}
	if raw {
		return lines.Raw(e.Ev), true
	}
	return lines.Compact(e.Ev, e.Cont)
}

// marshal is v's JSON, without HTML escaping (the lead reads it).
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
