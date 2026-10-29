package leadmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/msg/internal/feed"
	"github.com/bpalermo/redoma/msg/internal/leadtest"
)

type session struct {
	t     *testing.T
	in    *io.PipeWriter
	out   *bufio.Reader
	fake  *leadtest.Fake
	watch chan *relayv1.WatchEvent
	srv   *Server
	n     int
}

func start(t *testing.T) *session {
	t.Helper()
	c, fake := leadtest.Client(t)
	watch := make(chan *relayv1.WatchEvent)
	fake.Watches <- watch
	f := feed.New(c, feed.DefaultConfig)
	ctx, cancel := context.WithCancel(context.Background())
	go f.Run(ctx)
	t.Cleanup(cancel)
	srv := New(c, f, "test")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		_ = srv.MCP().Serve(ctx, inR, outW)
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return &session{t: t, in: inW, out: bufio.NewReader(outR), fake: fake, watch: watch, srv: srv}
}

// call calls a tool and returns its text and whether it is an error.
func (s *session) call(name string, args any) (string, bool) {
	s.t.Helper()
	s.n++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.n, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatal(err)
	}
	c := make(chan []byte, 1)
	go func() {
		line, _ := s.out.ReadBytes('\n')
		c <- line
	}()
	var line []byte
	select {
	case line = <-c:
	case <-time.After(10 * time.Second):
		s.t.Fatalf("no answer to %s", name)
	}
	var m struct {
		ID     int
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
		Error *struct{ Message string }
	}
	if err := json.Unmarshal(line, &m); err != nil || m.ID != s.n {
		s.t.Fatalf("answer %q: %v", line, err)
	}
	if m.Error != nil {
		return m.Error.Message, true
	}
	if len(m.Result.Content) != 1 {
		s.t.Fatalf("answer %q", line)
	}
	return m.Result.Content[0].Text, m.Result.IsError
}

// poll calls poll and decodes its response.
func (s *session) poll(args map[string]any) response {
	s.t.Helper()
	text, isErr := s.call("poll", args)
	if isErr {
		s.t.Fatalf("poll: %s", text)
	}
	var r response
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		s.t.Fatalf("poll's response %q: %v", text, err)
	}
	return r
}

// events polls until n events have come, from cursor, and returns them as
// text and the last response, its Omitted the sum of all.
func (s *session) events(args map[string]any, n int) ([]string, response) {
	s.t.Helper()
	var got []string
	var r response
	omitted := 0
	for range 20 {
		args["wait_seconds"] = 5
		r = s.poll(args)
		omitted += r.Omitted
		for _, e := range r.Events {
			got = append(got, string(e))
		}
		args["cursor"] = r.Cursor
		if len(got) >= n {
			break
		}
	}
	r.Omitted = omitted
	return got, r
}

func TestSendReplyCancel(t *testing.T) {
	s := start(t)
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"send", map[string]any{"agent": "a", "text": "fix the build"}, "a/t1"},
		{"reply", map[string]any{"question": "a/q1", "text": "main"}, "answered"},
		{"cancel", map[string]any{"task": "a/t1"}, "cancelled"},
	} {
		if text, isErr := s.call(c.tool, c.args); isErr || text != c.want {
			t.Errorf("%s: %q (error %v), want %q", c.tool, text, isErr, c.want)
		}
	}
	got := strings.Join(s.fake.Got(), "|")
	for _, want := range []string{"send a fix the build", "reply a/q1 main", "cancel a/t1"} {
		if !strings.Contains(got, want) {
			t.Errorf("the relay got %q, want %q", got, want)
		}
	}
	// The relay's error is the call's.
	if text, isErr := s.call("send", map[string]any{"agent": "nobody", "text": "x"}); !isErr || !strings.Contains(text, "not in the allowlist") {
		t.Errorf("a refused send: %q (error %v)", text, isErr)
	}
}

// TestBadInputs: what the relay would refuse is refused before it, and an
// argument a tool does not take too.
func TestBadInputs(t *testing.T) {
	s := start(t)
	// The feed's own Watch aside.
	calls := func() []string { return slices.DeleteFunc(s.fake.Got(), func(c string) bool { return c == "watch" }) }
	before := len(calls())
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"send", map[string]any{"agent": "A b", "text": "x"}},
		{"send", map[string]any{"agent": "a", "text": ""}},
		{"send", map[string]any{"agent": "a", "text": strings.Repeat("x", MaxText+1)}},
		{"send", map[string]any{"agent": "a", "text": "x", "priority": 1}},
		{"reply", map[string]any{"question": "a", "text": "x"}},
		{"reply", map[string]any{"question": "a/q 1", "text": "x"}},
		{"cancel", map[string]any{"task": "a/" + strings.Repeat("t", 65)}},
		{"poll", map[string]any{"agent": "../a"}},
		{"poll", map[string]any{"cursor": "nonsense"}},
		{"poll", map[string]any{"wait_seconds": -1}},
		{"poll", map[string]any{"raw": "yes"}},
	} {
		if text, isErr := s.call(c.tool, c.args); !isErr {
			t.Errorf("%s %v: %q, want an error", c.tool, c.args, text)
		}
	}
	if got := calls()[before:]; len(got) != 0 {
		t.Errorf("the relay was called: %q", got)
	}
}

// TestPollCompactAndRaw: compact output keeps the assistant's text, its
// tools, results, ends, questions and messages, and counts what it leaves
// out; raw gives every event whole.
func TestPollCompactAndRaw(t *testing.T) {
	s := start(t)
	sysInit := `{"type":"system","subtype":"init","tools":["` + strings.Repeat("Bash", 1000) + `"]}`
	for _, ev := range []*relayv1.WatchEvent{
		leadtest.Turn("a", "a/t1", sysInit),
		leadtest.Turn("a", "a/t1", `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Looking."},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`),
		leadtest.Turn("a", "a/t1", `{"type":"user","message":{"content":[{"type":"tool_result","content":"a lot"}]}}`),
		leadtest.Question("a", "a/q1", "a/t1", "which branch?"),
		leadtest.Message("a", "a/t1", "halfway"),
		leadtest.Turn("a", "a/t1", `{"type":"result","subtype":"success","is_error":false,"result":"Done.","total_cost_usd":0.1}`),
		{Agent: "a", Body: &relayv1.WatchEvent_TurnEvent{TurnEvent: &relayv1.TurnEvent{TaskId: "a/t1", Done: true}}},
	} {
		s.watch <- ev
	}
	got, r := s.events(map[string]any{}, 5)
	want := []string{
		`{"agent":"a","turn":{"task":"a/t1","text":"Looking.","tools":["Bash"]}}`,
		`{"agent":"a","question":{"id":"a/q1","task":"a/t1","text":"which branch?"}}`,
		`{"agent":"a","message":{"task":"a/t1","text":"halfway"}}`,
		`{"agent":"a","turn":{"task":"a/t1","result":"Done."}}`,
		`{"agent":"a","turn":{"task":"a/t1","done":true}}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("compact:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.Omitted != 2 || r.Watch != "connected" {
		t.Errorf("omitted %d, watch %q; want 2, connected", r.Omitted, r.Watch)
	}
	got, _ = s.events(map[string]any{"raw": true}, 7)
	if len(got) != 7 || got[0] != `{"agent":"a","turn":{"task":"a/t1","event":`+sysInit+`}}` {
		t.Errorf("raw: %d events, first %.100q", len(got), got[0])
	}
}

// TestPollPages: a response stays under max_bytes; the rest follows from
// its cursor with nothing lost or repeated, and an event over the bound
// alone is cut to fit.
func TestPollPages(t *testing.T) {
	s := start(t)
	text := strings.Repeat("y", 1500)
	for i := range 10 {
		s.watch <- leadtest.Message("a", "a/t1", fmt.Sprintf("%d %s", i, text))
	}
	s.watch <- leadtest.Turn("a", "a/t1", `{"type":"assistant","message":{"content":[{"type":"text","text":"`+strings.Repeat("é", 4000)+`"}]}}`)
	args := map[string]any{"max_bytes": MinMaxBytes}
	var got []string
	for range 20 {
		args["wait_seconds"] = 5
		text, _ := s.call("poll", args)
		if len(text) > MinMaxBytes {
			t.Errorf("a response of %d bytes, over %d", len(text), MinMaxBytes)
		}
		var r response
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			t.Fatal(err)
		}
		for _, e := range r.Events {
			got = append(got, string(e))
		}
		args["cursor"] = r.Cursor
		if len(got) == 11 {
			break
		}
	}
	if len(got) != 11 {
		t.Fatalf("%d events, want 11", len(got))
	}
	for i, e := range got[:10] {
		if !strings.Contains(e, fmt.Sprintf(`"text":"%d y`, i)) {
			t.Errorf("event %d: %.60q", i, e)
		}
	}
	var last struct {
		Turn      struct{ Text string }
		Truncated bool
	}
	if err := json.Unmarshal([]byte(got[10]), &last); err != nil || !last.Truncated || last.Turn.Text == "" || !strings.HasPrefix(strings.Repeat("é", 4000), last.Turn.Text) {
		t.Errorf("the event over the bound: truncated %v, %d bytes of text, err %v", last.Truncated, len(last.Turn.Text), err)
	}
}

// TestPollFilteredCursorUnfiltered: the cursor of a poll of one agent,
// used for every agent, still shows the question of another that the
// first passed over; used for the same agent, only what is new.
func TestPollFilteredCursorUnfiltered(t *testing.T) {
	s := start(t)
	s.watch <- leadtest.Question("b", "b/q1", "b/t1", "which?")
	s.watch <- leadtest.Message("a", "a/t1", "a1")
	gotA, r := s.events(map[string]any{"agent": "a"}, 1)
	if len(gotA) != 1 || !strings.Contains(gotA[0], `"a1"`) {
		t.Fatalf("agent a: %q", gotA)
	}
	filtered := r.Cursor
	got, _ := s.events(map[string]any{"cursor": filtered}, 1)
	if len(got) != 1 || !strings.Contains(got[0], `"id":"b/q1"`) {
		t.Errorf("all from a's cursor: %q, want b's question only, not a1 again", got)
	}
	s.watch <- leadtest.Message("a", "a/t1", "a2")
	if got, _ := s.events(map[string]any{"agent": "a", "cursor": filtered}, 1); len(got) != 1 || !strings.Contains(got[0], `"a2"`) {
		t.Errorf("agent a again: %q, want only a2", got)
	}
}

// TestPollHeldNeverCut: a question over max_bytes comes whole, alone.
func TestPollHeldNeverCut(t *testing.T) {
	s := start(t)
	text := strings.Repeat("q", 16<<10)
	s.watch <- leadtest.Question("a", "a/q1", "a/t1", text)
	s.watch <- leadtest.Message("a", "a/t1", "next")
	got, _ := s.events(map[string]any{"max_bytes": MinMaxBytes}, 1)
	var q struct {
		Question  struct{ Text string }
		Truncated bool
	}
	if len(got) != 1 || json.Unmarshal([]byte(got[0]), &q) != nil || q.Question.Text != text || q.Truncated {
		t.Errorf("the question: %d events, %d bytes of text, truncated %v; want it whole and alone", len(got), len(q.Question.Text), q.Truncated)
	}
}

// TestPollCursorOfAnotherProcess: a cursor from another redoma mcp
// starts over, and says so.
func TestPollCursorOfAnotherProcess(t *testing.T) {
	s := start(t)
	s.watch <- leadtest.Message("a", "a/t1", "hi")
	got, r := s.events(map[string]any{}, 1)
	if len(got) != 1 || r.Reset {
		t.Fatalf("got %q, reset %v", got, r.Reset)
	}
	// Of this format, and of an older one, which named the agent.
	for _, c := range []string{"00000000.1", "00000000.5.2.beta"} {
		r = s.poll(map[string]any{"cursor": c})
		if !r.Reset || len(r.Events) != 1 || !strings.HasPrefix(r.Cursor, s.srv.epoch+".") {
			t.Errorf("another process's cursor %q: %+v", c, r)
		}
	}
	// This process's, in a format it does not write.
	if text, isErr := s.call("poll", map[string]any{"cursor": s.srv.epoch + ".5.2.beta"}); !isErr {
		t.Errorf("a cursor of 4 parts of this process: %q, want an error", text)
	}
}

// TestList: the four tools, with schemas.
func TestList(t *testing.T) {
	var names []string
	for _, tl := range New(nil, nil, "test").MCP().Tools {
		if tl.InputSchema["type"] != "object" {
			t.Errorf("%s: schema %v", tl.Name, tl.InputSchema)
		}
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "send,reply,cancel,poll" {
		t.Errorf("tools %q", names)
	}
}
