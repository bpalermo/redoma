package msg

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
)

// fake is a Lead service that records what it is asked.
type fake struct {
	relayv1.UnimplementedLeadServer
	got []string
}

func (f *fake) Send(_ context.Context, r *relayv1.SendRequest) (*relayv1.SendResponse, error) {
	f.got = append(f.got, "send "+r.GetAgent()+" "+r.GetText())
	return relayv1.SendResponse_builder{TaskId: r.GetAgent() + "/t1"}.Build(), nil
}

func (f *fake) Reply(_ context.Context, r *relayv1.ReplyRequest) (*relayv1.ReplyResponse, error) {
	f.got = append(f.got, "reply "+r.GetQuestionId()+" "+r.GetText())
	return relayv1.ReplyResponse_builder{}.Build(), nil
}

func (f *fake) Cancel(_ context.Context, r *relayv1.CancelRequest) (*relayv1.CancelResponse, error) {
	f.got = append(f.got, "cancel "+r.GetTaskId())
	return relayv1.CancelResponse_builder{}.Build(), nil
}

func (f *fake) Watch(r *relayv1.WatchRequest, st relayv1.Lead_WatchServer) error {
	f.got = append(f.got, "watch "+r.GetAgent())
	for _, ev := range []*relayv1.WatchEvent{
		relayv1.WatchEvent_builder{Agent: "a", TurnEvent: relayv1.TurnEvent_builder{TaskId: "a/t1", Chunk: []byte(`{"type":"result"}`)}.Build()}.Build(),
		relayv1.WatchEvent_builder{Agent: "a", TurnEvent: relayv1.TurnEvent_builder{TaskId: "a/t1", Chunk: []byte(`not json`)}.Build()}.Build(),
		relayv1.WatchEvent_builder{Agent: "a", TurnEvent: relayv1.TurnEvent_builder{TaskId: "a/t1", Done: true, Error: "cancelled"}.Build()}.Build(),
		relayv1.WatchEvent_builder{Agent: "a", Question: relayv1.Question_builder{Id: "a/q1", Text: "which?", TaskId: "a/t1"}.Build()}.Build(),
		relayv1.WatchEvent_builder{Agent: "a", Message: relayv1.Message_builder{To: "lead", Text: "hi"}.Build()}.Build(),
		relayv1.WatchEvent_builder{Agent: "a", Dropped: relayv1.Dropped_builder{TurnEvents: 3, Reason: "over its rate"}.Build()}.Build(),
	} {
		if err := st.Send(ev); err != nil {
			return err
		}
	}
	return errors.New("the end")
}

func client(t *testing.T) (relayv1.LeadClient, *fake) {
	t.Helper()
	l := bufconn.Listen(1 << 16)
	f := &fake{}
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

func TestCommands(t *testing.T) {
	c, f := client(t)
	var out bytes.Buffer
	for _, args := range [][]string{
		{"send", "a", "fix", "the", "build"},
		{"send", "a", "-"},
		{"reply", "a/q1", "main"},
		{"cancel", "a/t1"},
	} {
		if err := run(context.Background(), c, args, strings.NewReader("from stdin\n"), &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	want := []string{"send a fix the build", "send a from stdin\n", "reply a/q1 main", "cancel a/t1"}
	if strings.Join(f.got, "|") != strings.Join(want, "|") {
		t.Errorf("asked %q, want %q", f.got, want)
	}
	if out.String() != "a/t1\na/t1\n" {
		t.Errorf("printed %q, want the task ids", out.String())
	}
}

// TestWatchPrintsJSONLines: one object per event, a JSON chunk embedded as
// JSON and any other chunk as text.
func TestWatchPrintsJSONLines(t *testing.T) {
	c, _ := client(t)
	var out bytes.Buffer
	err := run(context.Background(), c, []string{"watch", "a"}, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "the end") {
		t.Errorf("watch: %v, want the stream's error", err)
	}
	want := `{"agent":"a","turn":{"task":"a/t1","event":{"type":"result"}}}
{"agent":"a","turn":{"task":"a/t1","text":"not json"}}
{"agent":"a","turn":{"task":"a/t1","done":true,"error":"cancelled"}}
{"agent":"a","question":{"id":"a/q1","task":"a/t1","text":"which?"}}
{"agent":"a","message":{"text":"hi"}}
{"agent":"a","dropped":{"turn_events":3,"reason":"over its rate"}}
`
	if out.String() != want {
		t.Errorf("printed\n%s\nwant\n%s", out.String(), want)
	}
}

func TestUsage(t *testing.T) {
	c, _ := client(t)
	for _, args := range [][]string{nil, {"send", "a"}, {"reply", "a/q"}, {"cancel"}, {"watch", "a", "b"}, {"mcp", "x"}, {"frob"}} {
		if err := run(context.Background(), c, args, nil, &bytes.Buffer{}); !errors.Is(err, errUsage) {
			t.Errorf("%v: %v, want a usage error", args, err)
		}
	}
}
