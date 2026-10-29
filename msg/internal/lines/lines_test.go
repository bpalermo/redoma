package lines

import (
	"encoding/json"
	"strings"
	"testing"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
)

func turn(chunk string, partial bool) *relayv1.WatchEvent {
	return relayv1.WatchEvent_builder{Agent: "a", TurnEvent: relayv1.TurnEvent_builder{TaskId: "a/t1", Chunk: []byte(chunk), Partial: partial}.Build()}.Build()
}

func compact(t *testing.T, ev *relayv1.WatchEvent, cont bool) string {
	t.Helper()
	l, ok := Compact(ev, cont)
	if !ok {
		return "-"
	}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCompact: what compact output keeps of each kind of chunk.
func TestCompact(t *testing.T) {
	for _, c := range []struct {
		name string
		ev   *relayv1.WatchEvent
		cont bool
		want string
	}{
		{"system", turn(`{"type":"system","subtype":"init"}`, false), false, "-"},
		{"thinking only", turn(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"x"}]}}`, false), false, "-"},
		{
			"text and tools", turn(`{"type":"assistant","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"tool_use","name":"Read"}]}}`, false), false,
			`{"agent":"a","turn":{"task":"a/t1","text":"a\nb","tools":["Read"]}}`,
		},
		{
			"error result", turn(`{"type":"result","subtype":"error_max_turns","is_error":true}`, false), false,
			`{"agent":"a","turn":{"task":"a/t1","result":"error_max_turns","is_error":true}}`,
		},
		{"not json", turn(`warning: something`, false), false, `{"agent":"a","turn":{"task":"a/t1","text":"warning: something"}}`},
		{"a part", turn(`{"type":"assistant","mess`, true), false, "-"},
		{"the last part", turn(`age":{}}`, false), true, "-"},
		{
			"end", relayv1.WatchEvent_builder{Agent: "a", TurnEvent: relayv1.TurnEvent_builder{TaskId: "a/t1", Done: true, Error: "cancelled"}.Build()}.Build(), false,
			`{"agent":"a","turn":{"task":"a/t1","done":true,"error":"cancelled"}}`,
		},
	} {
		if got := compact(t, c.ev, c.cont); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// TestRawPartial: a part of a line is text, never embedded.
func TestRawPartial(t *testing.T) {
	l := Raw(turn(`{}`, true))
	if l.Turn.Event != nil || l.Turn.Text != "{}" || !l.Turn.Partial {
		t.Errorf("a partial chunk: %+v", l.Turn)
	}
}

// TestTruncate: the longest text is cut on a rune boundary; a JSON event
// becomes text first.
func TestTruncate(t *testing.T) {
	l := Line{Agent: "a", Turn: &Turn{Task: "a/t1", Event: json.RawMessage(`{"x":"` + strings.Repeat("é", 10) + `"}`)}}
	if !l.Truncate(5) || !l.Truncated || l.Turn.Event != nil || !strings.HasPrefix(`{"x":"`+strings.Repeat("é", 10), l.Turn.Text) {
		t.Errorf("truncated: %+v", l)
	}
	if !json.Valid(mustMarshal(t, l)) {
		t.Errorf("not JSON after the cut")
	}
	empty := Line{Agent: "a", Message: &Text{}}
	if empty.Truncate(1) {
		t.Errorf("an empty text truncated")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
