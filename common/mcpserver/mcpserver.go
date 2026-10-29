// Package mcpserver is the MCP server both of the repo's MCP servers run on
// stdio: redoma-shim's (claude's tools to reach the lead) and redoma msg's
// (the lead's tools to reach the agents). It serves tools only.
//
// The protocol is JSON-RPC 2.0, one message per line: initialize, ping,
// tools/list, tools/call and notifications/cancelled are served; any other
// request is "method not found". Each call runs on its own goroutine, so a
// call that waits (ask_lead, poll) leaves the others served.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
)

// protocolVersion is the MCP version answered when the client names none.
const protocolVersion = "2025-06-18"

// maxLine bounds one message from the client.
const maxLine = 4 << 20

// Tool is one tool: what tools/list shows, and Call, which gets the call's
// arguments as sent and returns its text. An error is the call's result,
// with isError set.
type Tool struct {
	Name        string                                                          `json:"name"`
	Description string                                                          `json:"description"`
	InputSchema map[string]any                                                  `json:"inputSchema"`
	Call        func(ctx context.Context, args json.RawMessage) (string, error) `json:"-"`
}

// Server serves one MCP session.
type Server struct {
	// Name and Version are the serverInfo answered to initialize.
	Name    string
	Version string
	Tools   []Tool

	mu      sync.Mutex
	w       io.Writer
	calls   map[string]context.CancelFunc
	pending sync.WaitGroup
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests from r and answers on w until r ends, then waits for
// the calls in flight.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.w, s.calls = w, map[string]context.CancelFunc{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			s.send(message{Error: &rpcError{Code: -32700, Message: "parse error"}, ID: json.RawMessage("null")})
			continue
		}
		s.dispatch(ctx, m)
	}
	s.pending.Wait()
	return sc.Err()
}

func (s *Server) send(m message) {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write(append(b, '\n'))
}

func (s *Server) result(id json.RawMessage, v any) { s.send(message{ID: id, Result: v}) }

func (s *Server) fail(id json.RawMessage, code int, msg string) {
	s.send(message{ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) dispatch(ctx context.Context, m message) {
	notification := len(m.ID) == 0
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = protocolVersion
		}
		s.result(m.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		})
	case "ping":
		s.result(m.ID, map[string]any{})
	case "tools/list":
		s.result(m.ID, map[string]any{"tools": s.Tools})
	case "tools/call":
		s.call(ctx, m)
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		if cancel, ok := s.calls[string(p.RequestID)]; ok {
			cancel()
		}
		s.mu.Unlock()
	default:
		if !notification {
			s.fail(m.ID, -32601, "method not found: "+m.Method)
		}
	}
}

// call runs a tool call on its own goroutine.
func (s *Server) call(ctx context.Context, m message) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.fail(m.ID, -32602, "invalid params")
		return
	}
	var t *Tool
	for i := range s.Tools {
		if s.Tools[i].Name == p.Name {
			t = &s.Tools[i]
		}
	}
	if t == nil {
		s.fail(m.ID, -32602, "no such tool: "+p.Name)
		return
	}
	if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
		p.Arguments = json.RawMessage("{}")
	}
	ctx, cancel := context.WithCancel(ctx)
	key := string(m.ID)
	s.mu.Lock()
	s.calls[key] = cancel
	s.mu.Unlock()
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		defer func() {
			s.mu.Lock()
			delete(s.calls, key)
			s.mu.Unlock()
			cancel()
		}()
		text, err := t.Call(ctx, p.Arguments)
		if ctx.Err() != nil {
			// Cancelled: MCP sends no response to a cancelled request.
			return
		}
		res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		if err != nil {
			res = map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
		}
		s.result(m.ID, res)
	}()
}
