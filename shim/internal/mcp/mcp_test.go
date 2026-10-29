package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// lead answers an ask with its reply, once released.
type lead struct {
	asked    chan string
	reply    chan string
	messages chan string
}

func (l *lead) Ask(ctx context.Context, q string) (string, error) {
	l.asked <- q
	select {
	case r := <-l.reply:
		return r, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (l *lead) Message(_ context.Context, text string) error {
	l.messages <- text
	return nil
}

type session struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	lead *lead
	done chan error
}

func start(t *testing.T) *session {
	t.Helper()
	l := &lead{asked: make(chan string, 4), reply: make(chan string, 4), messages: make(chan string, 4)}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, in: inW, out: bufio.NewReader(outR), lead: l, done: make(chan error, 1)}
	go func() {
		s.done <- (&Server{Lead: l, Version: "test"}).Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return s
}

func (s *session) send(m string) {
	s.t.Helper()
	if _, err := s.in.Write([]byte(m + "\n")); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) recv() map[string]any {
	s.t.Helper()
	c := make(chan map[string]any, 1)
	go func() {
		line, err := s.out.ReadBytes('\n')
		if err != nil {
			c <- nil
			return
		}
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		c <- m
	}()
	select {
	case m := <-c:
		if m == nil {
			s.t.Fatal("the server closed its output")
		}
		return m
	case <-time.After(5 * time.Second):
		s.t.Fatal("no answer")
		return nil
	}
}

func text(m map[string]any) string {
	res, _ := m["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	c, _ := content[0].(map[string]any)
	s, _ := c["text"].(string)
	return s
}

func TestInitializeAndList(t *testing.T) {
	s := start(t)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	m := s.recv()
	res := m["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" || m["id"].(float64) != 1 {
		t.Errorf("initialize: %v", m)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("no tools capability: %v", res)
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	m = s.recv()
	var names []string
	for _, tl := range m["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "ask_lead,send_message" {
		t.Errorf("tools %q", names)
	}
	s.send(`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)
	if m := s.recv(); m["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("an unknown method: %v", m)
	}
	s.send(`not json`)
	if m := s.recv(); m["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Errorf("a parse error: %v", m)
	}
}

// TestAskLeadBlocks: ask_lead answers once the lead has, and the server
// serves other requests meanwhile.
func TestAskLeadBlocks(t *testing.T) {
	s := start(t)
	s.send(`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"ask_lead","arguments":{"question":"which?"}}}`)
	if q := <-s.lead.asked; q != "which?" {
		t.Errorf("asked %q", q)
	}
	s.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	if m := s.recv(); m["id"] != "p" {
		t.Errorf("a ping while ask_lead waits: %v", m)
	}
	s.lead.reply <- "this one"
	m := s.recv()
	if m["id"] != "a" || text(m) != "this one" {
		t.Errorf("ask_lead: %v", m)
	}
	s.send(`{"jsonrpc":"2.0","id":"m","method":"tools/call","params":{"name":"send_message","arguments":{"text":"fyi"}}}`)
	if m := s.recv(); text(m) != "sent" || <-s.lead.messages != "fyi" {
		t.Errorf("send_message: %v", m)
	}
	s.send(`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"ask_lead","arguments":{}}}`)
	if m := s.recv(); m["result"].(map[string]any)["isError"] != true {
		t.Errorf("ask_lead without a question: %v", m)
	}
}

// TestCancelledCall: a cancelled ask_lead stops waiting and is not
// answered.
func TestCancelledCall(t *testing.T) {
	s := start(t)
	s.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"ask_lead","arguments":{"question":"q"}}}`)
	<-s.lead.asked
	s.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	s.send(`{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	if m := s.recv(); m["id"].(float64) != 8 {
		t.Errorf("the cancelled call was answered: %v", m)
	}
	s.in.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not end with its input")
	}
}
