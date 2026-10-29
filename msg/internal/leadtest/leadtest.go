// Package leadtest is a fake of the relay's Lead service, over bufconn,
// for redoma msg's tests.
package leadtest

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
)

// Fake records the calls it gets. Each Watch takes the next channel off
// Watches and sends what comes on it; a closed channel breaks the Watch
// (Unavailable).
type Fake struct {
	relayv1.UnimplementedLeadServer
	Watches chan chan *relayv1.WatchEvent

	mu  sync.Mutex
	got []string
}

func (f *Fake) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, s)
}

// Got is the calls so far, one string each.
func (f *Fake) Got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// Send answers <agent>/t1, or NotFound for the agent "nobody".
func (f *Fake) Send(_ context.Context, r *relayv1.SendRequest) (*relayv1.SendResponse, error) {
	f.record("send " + r.GetAgent() + " " + r.GetText())
	if r.GetAgent() == "nobody" {
		return nil, status.Error(codes.NotFound, `agent "nobody" is not in the allowlist`)
	}
	return relayv1.SendResponse_builder{TaskId: r.GetAgent() + "/t1"}.Build(), nil
}

// Reply records.
func (f *Fake) Reply(_ context.Context, r *relayv1.ReplyRequest) (*relayv1.ReplyResponse, error) {
	f.record("reply " + r.GetQuestionId() + " " + r.GetText())
	return relayv1.ReplyResponse_builder{}.Build(), nil
}

// Cancel records.
func (f *Fake) Cancel(_ context.Context, r *relayv1.CancelRequest) (*relayv1.CancelResponse, error) {
	f.record("cancel " + r.GetTaskId())
	return relayv1.CancelResponse_builder{}.Build(), nil
}

// Watch sends what comes on the next of Watches.
func (f *Fake) Watch(r *relayv1.WatchRequest, st relayv1.Lead_WatchServer) error {
	f.record(strings.TrimSpace("watch " + r.GetAgent()))
	var ch chan *relayv1.WatchEvent
	select {
	case ch = <-f.Watches:
	case <-st.Context().Done():
		return nil
	}
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return status.Error(codes.Unavailable, "the watch broke")
			}
			if err := st.Send(ev); err != nil {
				return err
			}
		case <-st.Context().Done():
			return nil
		}
	}
}

// Client serves a new Fake and returns a client of it.
func Client(t testing.TB) (relayv1.LeadClient, *Fake) {
	t.Helper()
	l := bufconn.Listen(1 << 20)
	f := &Fake{Watches: make(chan chan *relayv1.WatchEvent, 4)}
	g := grpc.NewServer()
	relayv1.RegisterLeadServer(g, f)
	go func() { _ = g.Serve(l) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///lead",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return relayv1.NewLeadClient(conn), f
}

// Turn is a turn event of agent's task with chunk.
func Turn(agent, task, chunk string) *relayv1.WatchEvent {
	return relayv1.WatchEvent_builder{Agent: agent, TurnEvent: relayv1.TurnEvent_builder{TaskId: task, Chunk: []byte(chunk)}.Build()}.Build()
}

// Question is agent's question id (<agent>/<id>) of task.
func Question(agent, id, task, text string) *relayv1.WatchEvent {
	return relayv1.WatchEvent_builder{Agent: agent, Question: relayv1.Question_builder{Id: id, TaskId: task, Text: text}.Build()}.Build()
}

// Message is agent's message of task.
func Message(agent, task, text string) *relayv1.WatchEvent {
	return relayv1.WatchEvent_builder{Agent: agent, Message: relayv1.Message_builder{To: "lead", TaskId: task, Text: text}.Build()}.Build()
}
