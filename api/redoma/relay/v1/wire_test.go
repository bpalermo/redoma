package relayv1_test

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
)

// wire are the messages of testdata/wire.txt, whose bytes were captured
// from this package when relay.proto was syntax proto3, before it moved to
// edition 2024. Zero values (an empty error, partial false, seq 0) are among
// them: under implicit presence they are not on the wire.
var wire = map[string]proto.Message{
	"envelope_task":               relayv1.Envelope_builder{Seq: 7, Task: relayv1.Task_builder{Id: "a/1", Text: "do it"}.Build()}.Build(),
	"envelope_reply":              relayv1.Envelope_builder{Seq: 8, Reply: relayv1.Reply_builder{QuestionId: "q1", Text: "yes"}.Build()}.Build(),
	"envelope_cancel":             relayv1.Envelope_builder{Seq: 9, Cancel: relayv1.Cancel_builder{TaskId: "a/1"}.Build()}.Build(),
	"envelope_ack":                relayv1.Envelope_builder{Ack: relayv1.Ack_builder{Seq: 300}.Build()}.Build(),
	"envelope_ack_zero":           relayv1.Envelope_builder{Ack: relayv1.Ack_builder{}.Build()}.Build(),
	"envelope_turn_event":         relayv1.Envelope_builder{Seq: 1, TurnEvent: relayv1.TurnEvent_builder{TaskId: "1", Chunk: []byte(`{"type":"result"}`), Done: true, Error: "cancelled", Partial: false}.Build()}.Build(),
	"envelope_turn_event_partial": relayv1.Envelope_builder{Seq: 2, TurnEvent: relayv1.TurnEvent_builder{TaskId: "1", Chunk: []byte("x"), Partial: true}.Build()}.Build(),
	"envelope_turn_event_empty":   relayv1.Envelope_builder{Seq: 3, TurnEvent: relayv1.TurnEvent_builder{}.Build()}.Build(),
	"envelope_question":           relayv1.Envelope_builder{Seq: 4, Question: relayv1.Question_builder{Id: "q1", Text: "which?", TaskId: "1"}.Build()}.Build(),
	"envelope_message":            relayv1.Envelope_builder{Seq: 5, Message: relayv1.Message_builder{To: "lead", Text: "hi", TaskId: "1"}.Build()}.Build(),
	"envelope_heartbeat":          relayv1.Envelope_builder{Heartbeat: relayv1.Heartbeat_builder{}.Build()}.Build(),
	"envelope_empty":              relayv1.Envelope_builder{}.Build(),
	"send_request":                relayv1.SendRequest_builder{Agent: "a", Text: "t"}.Build(),
	"send_response":               relayv1.SendResponse_builder{TaskId: "a/1"}.Build(),
	"reply_request":               relayv1.ReplyRequest_builder{QuestionId: "a/q1", Text: "yes"}.Build(),
	"cancel_request":              relayv1.CancelRequest_builder{TaskId: "a/1"}.Build(),
	"watch_request_all":           relayv1.WatchRequest_builder{}.Build(),
	"watch_event_question":        relayv1.WatchEvent_builder{Agent: "a", Question: relayv1.Question_builder{Id: "a/q1", Text: "which?", TaskId: "a/1"}.Build()}.Build(),
	"watch_event_dropped":         relayv1.WatchEvent_builder{Agent: "a", Dropped: relayv1.Dropped_builder{TurnEvents: 3, Invalid: 1, Reason: "over its rate"}.Build()}.Build(),
	"watch_event_turn_event":      relayv1.WatchEvent_builder{Agent: "a", TurnEvent: relayv1.TurnEvent_builder{TaskId: "a/1", Chunk: []byte(`{"type":"assistant"}`), Partial: true}.Build()}.Build(),
	"watch_event_message":         relayv1.WatchEvent_builder{Agent: "a", Message: relayv1.Message_builder{To: "lead", Text: "done", TaskId: "a/1"}.Build()}.Build(),
	"watch_event_dropped_all":     relayv1.WatchEvent_builder{Agent: "b", Dropped: relayv1.Dropped_builder{TurnEvents: 1, Questions: 2, Messages: 3, Invalid: 4, Reason: "over a limit"}.Build()}.Build(),
	"watch_request_agent":         relayv1.WatchRequest_builder{Agent: "a"}.Build(),
}

// captured reads testdata/wire.txt: one "<name> <hex>" per line.
func captured(t *testing.T) map[string][]byte {
	t.Helper()
	f, err := os.Open("testdata/wire.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]byte{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, h, _ := strings.Cut(sc.Text(), " ")
		raw, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = raw
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestWireUnchanged: each message encodes to the bytes the proto3 version
// encoded it to, and those bytes decode to the same message, so a peer of
// either version reads the other.
func TestWireUnchanged(t *testing.T) {
	golden := captured(t)
	if len(golden) != len(wire) {
		t.Fatalf("testdata has %d messages, the test %d", len(golden), len(wire))
	}
	for name, m := range wire {
		want, ok := golden[name]
		if !ok {
			t.Errorf("%s: not in testdata", name)
			continue
		}
		got, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != hex.EncodeToString(want) {
			t.Errorf("%s: encodes to %x, the proto3 version to %x", name, got, want)
		}
		back := m.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(want, back); err != nil {
			t.Errorf("%s: %v", name, err)
		} else if !proto.Equal(back, m) {
			t.Errorf("%s: the proto3 bytes decode to %v, want %v", name, back, m)
		}
	}
}

// TestImplicitPresence: a scalar set to its zero value is not on the wire
// and reads back as unset, as under proto3.
func TestImplicitPresence(t *testing.T) {
	ev := relayv1.TurnEvent_builder{TaskId: "1"}.Build()
	ev.SetDone(false)
	ev.SetError("")
	raw, err := proto.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(raw) != "0a0131" {
		t.Errorf("zero done and error are on the wire: %x", raw)
	}
}
