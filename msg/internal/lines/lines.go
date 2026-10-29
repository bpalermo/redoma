// Package lines is how redoma msg shows the relay's WatchEvents: Raw is
// the event whole (what `redoma msg watch` prints, and poll with raw), and
// Compact is what the lead reads by default: the agents' text, the tools
// they use, results, ends, questions and messages, without the stream-json
// around them.
package lines

import (
	"encoding/json"
	"strings"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
)

// Line is one event as redoma msg shows it. A turn's chunk, a line of
// claude's stream-json, is embedded as JSON when it is JSON.
type Line struct {
	Agent    string `json:"agent"`
	Turn     *Turn  `json:"turn,omitempty"`
	Question *Text  `json:"question,omitempty"`
	Message  *Text  `json:"message,omitempty"`
	Dropped  *Drop  `json:"dropped,omitempty"`
	// Truncated: a text of this line was cut to fit a response.
	Truncated bool `json:"truncated,omitempty"`
}

// Drop is what was dropped of the agent's (relayv1.Dropped).
type Drop struct {
	TurnEvents uint64 `json:"turn_events,omitempty"`
	Questions  uint64 `json:"questions,omitempty"`
	Messages   uint64 `json:"messages,omitempty"`
	Invalid    uint64 `json:"invalid,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// Turn is a turn's event. Raw sets Event (a JSON chunk) or Text (any other
// chunk) and Partial; Compact sets Text (the assistant's), Tools, Result
// and IsError from the stream-json.
type Turn struct {
	Task    string          `json:"task"`
	Event   json.RawMessage `json:"event,omitempty"`
	Text    string          `json:"text,omitempty"`
	Partial bool            `json:"partial,omitempty"`
	Tools   []string        `json:"tools,omitempty"`
	Result  string          `json:"result,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Done    bool            `json:"done,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Text is a question or a message.
type Text struct {
	ID   string `json:"id,omitempty"`
	Task string `json:"task,omitempty"`
	Text string `json:"text"`
}

// Raw is the event whole.
func Raw(ev *relayv1.WatchEvent) Line {
	l := Line{Agent: ev.GetAgent()}
	switch ev.WhichBody() {
	case relayv1.WatchEvent_TurnEvent_case:
		t := &Turn{Task: ev.GetTurnEvent().GetTaskId(), Partial: ev.GetTurnEvent().GetPartial(), Done: ev.GetTurnEvent().GetDone(), Error: ev.GetTurnEvent().GetError()}
		if chunk := ev.GetTurnEvent().GetChunk(); !t.Partial && json.Valid(chunk) {
			t.Event = chunk
		} else if len(chunk) > 0 {
			t.Text = string(chunk)
		}
		l.Turn = t
	case relayv1.WatchEvent_Question_case:
		l.Question = &Text{ID: ev.GetQuestion().GetId(), Task: ev.GetQuestion().GetTaskId(), Text: ev.GetQuestion().GetText()}
	case relayv1.WatchEvent_Message_case:
		l.Message = &Text{Task: ev.GetMessage().GetTaskId(), Text: ev.GetMessage().GetText()}
	case relayv1.WatchEvent_Dropped_case:
		d := ev.GetDropped()
		l.Dropped = &Drop{TurnEvents: d.GetTurnEvents(), Questions: d.GetQuestions(), Messages: d.GetMessages(), Invalid: d.GetInvalid(), Reason: d.GetReason()}
	}
	return l
}

// streamJSON is what Compact reads of a stream-json line.
type streamJSON struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// Compact is the event as the lead reads it by default, and false for one
// it leaves out: stream-json other than the assistant's text and tool uses
// and the result (system events, tool results, partial messages), and the
// parts of a line split over events (cont: the event goes on a line an
// earlier event began). Questions, messages and drops are whole.
func Compact(ev *relayv1.WatchEvent, cont bool) (Line, bool) {
	te := ev.GetTurnEvent()
	if te == nil {
		return Raw(ev), true
	}
	t := &Turn{Task: te.GetTaskId(), Done: te.GetDone(), Error: te.GetError()}
	if chunk := te.GetChunk(); len(chunk) > 0 && !te.GetPartial() && !cont {
		t.compact(chunk)
	}
	if t.Text == "" && len(t.Tools) == 0 && t.Result == "" && !t.IsError && !t.Done && t.Error == "" {
		return Line{}, false
	}
	return Line{Agent: ev.GetAgent(), Turn: t}, true
}

// compact sets what Compact keeps of a whole line of the turn's output.
func (t *Turn) compact(chunk []byte) {
	var s streamJSON
	switch {
	case json.Unmarshal(chunk, &s) != nil || s.Type == "":
		// Not stream-json: whatever claude printed besides.
		t.Text = string(chunk)
	case s.Type == "assistant":
		var text []string
		for _, c := range s.Message.Content {
			switch c.Type {
			case "text":
				text = append(text, c.Text)
			case "tool_use":
				t.Tools = append(t.Tools, c.Name)
			}
		}
		t.Text = strings.Join(text, "\n")
	case s.Type == "result":
		t.Result, t.IsError = s.Result, s.IsError
		if t.Result == "" && s.Subtype != "success" {
			t.Result = s.Subtype
		}
	}
}

// Truncate cuts the line's longest text by cut bytes (at least), on a rune
// boundary, and marks it truncated; it reports false when no text is left
// to cut.
func (l *Line) Truncate(cut int) bool {
	var fields []*string
	if l.Turn != nil {
		if len(l.Turn.Event) > 0 {
			// A JSON event cut is no longer JSON: it goes on as text.
			l.Turn.Text, l.Turn.Event = string(l.Turn.Event), nil
		}
		fields = append(fields, &l.Turn.Text, &l.Turn.Result, &l.Turn.Error)
	}
	for _, t := range []*Text{l.Question, l.Message} {
		if t != nil {
			fields = append(fields, &t.Text)
		}
	}
	var longest *string
	for _, f := range fields {
		if longest == nil || len(*f) > len(*longest) {
			longest = f
		}
	}
	if longest == nil || *longest == "" {
		return false
	}
	n := max(len(*longest)-cut, 0)
	*longest = strings.ToValidUTF8((*longest)[:n], "")
	l.Truncated = true
	return true
}
