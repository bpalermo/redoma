// Package mcp is the shim's MCP server, which claude runs on stdio for each
// turn (redoma-shim mcp). Its two tools reach the lead through the shim:
//
//   - ask_lead(question): asks the lead and returns the answer; the call
//     waits for it, however long that takes;
//   - send_message(text): tells the lead something, without waiting.
//
// The protocol is common/mcpserver's.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/bpalermo/redoma/common/mcpserver"
	"github.com/bpalermo/redoma/shim/internal/bridge"
)

func stringInput(name, description string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{name: map[string]any{"type": "string", "description": description}},
		"required":             []string{name},
		"additionalProperties": false,
	}
}

// Server serves one MCP session.
type Server struct {
	Lead    bridge.Lead
	Version string
}

// Serve reads requests from r and answers on w until r ends, then waits for
// the calls in flight.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	srv := &mcpserver.Server{Name: "redoma", Version: s.Version, Tools: []mcpserver.Tool{
		{
			Name: "ask_lead",
			Description: "Ask the lead a question and wait for the answer. Use it when you cannot go on " +
				"without a decision or information only the lead has; the call returns the lead's reply.",
			InputSchema: stringInput("question", "the question, complete enough to be answered without other context"),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				q, err := stringArg(args, "question")
				if err != nil {
					return "", err
				}
				return s.Lead.Ask(ctx, q)
			},
		},
		{
			Name:        "send_message",
			Description: "Tell the lead something (progress, a finding, a heads-up) without waiting for an answer.",
			InputSchema: stringInput("text", "the message"),
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				text, err := stringArg(args, "text")
				if err != nil {
					return "", err
				}
				if err := s.Lead.Message(ctx, text); err != nil {
					return "", err
				}
				return "sent", nil
			},
		},
	}}
	return srv.Serve(ctx, r, w)
}

// stringArg is the call's one argument, a string that must not be empty.
func stringArg(args json.RawMessage, name string) (string, error) {
	var a map[string]string
	if err := json.Unmarshal(args, &a); err != nil || a[name] == "" {
		return "", fmt.Errorf("a %s (a string) is required", name)
	}
	return a[name], nil
}

// Remote is a bridge.Lead at a bridge's address.
type Remote string

// Ask asks through the bridge.
func (r Remote) Ask(ctx context.Context, question string) (string, error) {
	return bridge.Ask(ctx, string(r), question)
}

// Message messages through the bridge.
func (r Remote) Message(ctx context.Context, text string) error {
	return bridge.Message(ctx, string(r), text)
}
