package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/common/reliable"
	"github.com/bpalermo/redoma/shim/internal/reaper"
)

// relay is a fake redoma-relay: the real at-least-once machinery, in
// memory, with what the shim sends collected on got. It can be stopped and
// started again, as a broken connection or a restart would.
type relay struct {
	t   *testing.T
	out *reliable.Outbox
	in  *reliable.Inbox
	got chan *relayv1.Envelope

	mu  sync.Mutex
	l   *bufconn.Listener
	srv *grpc.Server
}

type relayService struct {
	relayv1.UnimplementedRelayServer
	r *relay
}

func (s relayService) Connect(st relayv1.Relay_ConnectServer) error {
	md, _ := metadata.FromIncomingContext(st.Context())
	epoch := md.Get(reliable.EpochKey)
	if len(epoch) != 1 {
		return status.Error(codes.InvalidArgument, "no epoch")
	}
	if err := st.SendHeader(metadata.Pairs(reliable.EpochKey, s.r.out.Epoch())); err != nil {
		return err
	}
	sess := reliable.Session{
		Out: s.r.out, In: s.r.in, PeerEpoch: epoch[0], AckEvery: 5 * time.Millisecond,
		Handle: func(e *relayv1.Envelope) { s.r.got <- e },
	}
	return sess.Run(st.Context(), st)
}

func newRelay(t *testing.T) *relay {
	dir := t.TempDir()
	out, err := reliable.OpenOutbox(filepath.Join(dir, "out"), false)
	if err != nil {
		t.Fatal(err)
	}
	in, err := reliable.OpenInbox(filepath.Join(dir, "in.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{t: t, out: out, in: in, got: make(chan *relayv1.Envelope, 1024)}
	r.start()
	t.Cleanup(func() { r.stop(); out.Close() })
	return r
}

func (r *relay) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.l = bufconn.Listen(1 << 20)
	r.srv = grpc.NewServer()
	relayv1.RegisterRelayServer(r.srv, relayService{r: r})
	go func(srv *grpc.Server, l net.Listener) { _ = srv.Serve(l) }(r.srv, r.l)
}

func (r *relay) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srv.Stop()
}

func (r *relay) dial(ctx context.Context, _ string) (net.Conn, error) {
	r.mu.Lock()
	l := r.l
	r.mu.Unlock()
	return l.DialContext(ctx)
}

func (r *relay) push(e *relayv1.Envelope) {
	r.t.Helper()
	if err := r.out.Push(e); err != nil {
		r.t.Fatal(err)
	}
}

func (r *relay) task(id, text string) {
	r.t.Helper()
	r.push(relayv1.Envelope_builder{Task: relayv1.Task_builder{Id: id, Text: text}.Build()}.Build())
}

func (r *relay) cancel(id string) {
	r.t.Helper()
	r.push(relayv1.Envelope_builder{Cancel: relayv1.Cancel_builder{TaskId: id}.Build()}.Build())
}

// next returns the next envelope the shim sent.
func (r *relay) next() *relayv1.Envelope {
	r.t.Helper()
	select {
	case e := <-r.got:
		return e
	case <-time.After(10 * time.Second):
		r.t.Fatal("nothing from the shim")
		return nil
	}
}

// until returns the envelopes up to the first that cond accepts, which is
// last.
func (r *relay) until(what string, cond func(*relayv1.Envelope) bool) []*relayv1.Envelope {
	r.t.Helper()
	var out []*relayv1.Envelope
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-r.got:
			out = append(out, e)
			if cond(e) {
				return out
			}
		case <-deadline:
			r.t.Fatalf("timed out waiting for %s; got %v", what, out)
			return nil
		}
	}
}

// doneOf accepts the last TurnEvent of the task.
func doneOf(id string) func(*relayv1.Envelope) bool {
	return func(e *relayv1.Envelope) bool {
		return e.GetTurnEvent().GetTaskId() == id && e.GetTurnEvent().GetDone()
	}
}

// binary is a binary of the test's data, by the env var its path is in.
func binary(t *testing.T, env string) string {
	t.Helper()
	p := os.Getenv(env)
	if p == "" {
		t.Fatalf("%s is not set (the go_test's env)", env)
	}
	// A rootpath: relative to the main repository's runfiles, not to the
	// package directory the test runs in.
	return filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"), p)
}

type env struct {
	t     *testing.T
	relay *relay
	cfg   Config
	log   string
	pids  string

	stop func()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, relay: newRelay(t), log: filepath.Join(dir, "claude.log")}
	t.Setenv("FAKECLAUDE_LOG", e.log)
	e.pids = filepath.Join(dir, "child.pid")
	t.Setenv("FAKECLAUDE_PIDS", e.pids)
	rctx, rcancel := context.WithCancel(context.Background())
	t.Cleanup(rcancel)
	rp, err := reaper.New(rctx, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient("passthrough:///relay", grpc.WithContextDialer(e.relay.dial), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	e.cfg = Config{
		StateDir:       filepath.Join(work, ".redoma"),
		Workdir:        work,
		Claude:         binary(t, "FAKECLAUDE"),
		Self:           binary(t, "REDOMA_SHIM"),
		Relay:          relayv1.NewRelayClient(conn),
		AckEvery:       5 * time.Millisecond,
		HeartbeatEvery: time.Hour,
		MinBackoff:     10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
		KillGrace:      time.Second,
		Log:            slog.New(slog.DiscardHandler),
		Reaper:         rp,
	}
	return e
}

// start runs a shim on the env's state until the test ends or stop.
func (e *env) start() *Shim {
	t := e.t
	t.Helper()
	// The bridge on loopback TCP, which the tests' sandbox allows where it
	// refuses unix sockets; the daemon's is a unix socket (main).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	cfg.Bridge = l
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	var once sync.Once
	e.stop = func() {
		once.Do(func() {
			cancel()
			<-done
			s.Close()
		})
	}
	t.Cleanup(e.stop)
	return s
}

// runs are the argument lists fakeclaude was run with.
func (e *env) runs() [][]string {
	e.t.Helper()
	f, err := os.Open(e.log)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var args []string
		if err := json.Unmarshal(sc.Bytes(), &args); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, args)
	}
	return out
}

// The sessions fakeclaude knows (its NewSession and GoneSession).
const (
	newSession  = "5c3f8a2e-4b1d-4e6f-9a7c-2d8e0f1b3a4c"
	goneSession = "00000000-0000-4000-8000-000000000000"
)

// resumeOf is the session a run resumed: its --resume=<id>.
func resumeOf(args []string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--resume="); ok {
			return v
		}
		if a == "--resume" {
			return "(a separate argument)"
		}
	}
	return ""
}

func chunks(es []*relayv1.Envelope) []string {
	var out []string
	for _, e := range es {
		if c := e.GetTurnEvent().GetChunk(); len(c) > 0 {
			out = append(out, string(c))
		}
	}
	return out
}

// TestTurn: a task is one claude turn, its output streamed line by line and
// then its end; the session it started is resumed by the next turn.
func TestTurn(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "echo hello")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	lines := chunks(got)
	if len(lines) != 3 || !strings.Contains(lines[0], `"init"`) || !strings.Contains(lines[1], "hello") || !strings.Contains(lines[2], `"result"`) {
		t.Errorf("turn output %q, want init, hello, result", lines)
	}
	if last := got[len(got)-1].GetTurnEvent(); last.GetError() != "" {
		t.Errorf("t1 ended with %q", last.GetError())
	}
	for _, ev := range got {
		if ev.GetTurnEvent().GetTaskId() != "a/t1" {
			t.Errorf("an event of another task: %v", ev)
		}
	}

	e.relay.task("a/t2", "echo again")
	e.relay.until("t2 done", doneOf("a/t2"))
	runs := e.runs()
	if len(runs) != 2 {
		t.Fatalf("%d runs, want 2", len(runs))
	}
	if r := resumeOf(runs[0]); r != "" {
		t.Errorf("the first turn resumed %q", r)
	}
	if r := resumeOf(runs[1]); r != newSession {
		t.Errorf("the second turn resumed %q, want the first's session", r)
	}
	for _, want := range []string{"-p", "stream-json", "--verbose", "--mcp-config"} {
		if !strings.Contains(strings.Join(runs[0], " "), want) {
			t.Errorf("claude's arguments %q lack %q", runs[0], want)
		}
	}
}

// TestOneTurnAtATime: tasks run in order, the second after the first ends.
func TestOneTurnAtATime(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "slow 3")
	e.relay.task("a/t2", "echo second")
	got := e.relay.until("t2 done", doneOf("a/t2"))
	firstDone := -1
	for i, ev := range got {
		if doneOf("a/t1")(ev) {
			firstDone = i
		}
		if ev.GetTurnEvent().GetTaskId() == "a/t2" && firstDone < 0 {
			t.Fatalf("t2 output before t1 ended: %v", ev)
		}
	}
}

// TestCancelRunning: a cancel kills the running turn, which ends
// cancelled.
func TestCancelRunning(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "sleep")
	e.relay.until("t1's init", func(ev *relayv1.Envelope) bool { return len(ev.GetTurnEvent().GetChunk()) > 0 })
	start := time.Now()
	e.relay.cancel("a/t1")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "cancelled" {
		t.Errorf("t1 ended with %q, want cancelled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the cancel took %v", time.Since(start))
	}
	// The next task runs.
	e.relay.task("a/t2", "echo after")
	e.relay.until("t2 done", doneOf("a/t2"))
}

// TestCancelQueued: a cancelled task that has not started is dropped,
// reported cancelled, and never run.
func TestCancelQueued(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "sleep")
	e.relay.task("a/t2", "echo never")
	e.relay.until("t1's init", func(ev *relayv1.Envelope) bool { return len(ev.GetTurnEvent().GetChunk()) > 0 })
	e.relay.cancel("a/t2")
	got := e.relay.until("t2 done", doneOf("a/t2"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "cancelled" {
		t.Errorf("t2 ended with %q, want cancelled", err)
	}
	e.relay.cancel("a/t1")
	e.relay.until("t1 done", doneOf("a/t1"))
	if n := len(e.runs()); n != 1 {
		t.Errorf("%d runs, want only t1's", n)
	}
}

// TestAskLead: ask_lead sends a Question with the turn's task and blocks
// until the Reply, which the turn then has.
func TestAskLead(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "ask which branch?")
	got := e.relay.until("the question", func(ev *relayv1.Envelope) bool { return ev.GetQuestion() != nil })
	q := got[len(got)-1].GetQuestion()
	if q.GetText() != "which branch?" || q.GetTaskId() != "a/t1" || q.GetId() == "" {
		t.Errorf("question %v", q)
	}
	// It waits: nothing more until the reply.
	select {
	case ev := <-e.relay.got:
		t.Fatalf("the turn went on before the reply: %v", ev)
	case <-time.After(300 * time.Millisecond):
	}
	e.relay.push(relayv1.Envelope_builder{Reply: relayv1.Reply_builder{QuestionId: q.GetId(), Text: "use main"}.Build()}.Build())
	got = e.relay.until("t1 done", doneOf("a/t1"))
	if lines := strings.Join(chunks(got), "\n"); !strings.Contains(lines, "use main") {
		t.Errorf("the turn did not get the reply: %s", lines)
	}
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "" {
		t.Errorf("t1 ended with %q", err)
	}
}

// TestAskLeadSecondReply: a second reply to an answered question (the
// relay refuses one, NotFound, but one it resends after a reconnect still
// comes) is dropped: the turn has the first answer only, and the shim goes
// on.
func TestAskLeadSecondReply(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "ask which branch?")
	got := e.relay.until("the question", func(ev *relayv1.Envelope) bool { return ev.GetQuestion() != nil })
	q := got[len(got)-1].GetQuestion()
	e.relay.push(relayv1.Envelope_builder{Reply: relayv1.Reply_builder{QuestionId: q.GetId(), Text: "use main"}.Build()}.Build())
	e.relay.push(relayv1.Envelope_builder{Reply: relayv1.Reply_builder{QuestionId: q.GetId(), Text: "use dev"}.Build()}.Build())
	got = e.relay.until("t1 done", doneOf("a/t1"))
	if lines := strings.Join(chunks(got), "\n"); !strings.Contains(lines, "use main") || strings.Contains(lines, "use dev") {
		t.Errorf("the turn's answers: %s", lines)
	}
	e.relay.task("a/t2", "echo still here")
	if got := e.relay.until("t2 done", doneOf("a/t2")); got[len(got)-1].GetTurnEvent().GetError() != "" {
		t.Errorf("t2 after the second reply: %v", got[len(got)-1])
	}
}

// TestAskLeadCancelled: a cancel ends a turn that waits for a reply.
func TestAskLeadCancelled(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "ask anyone?")
	e.relay.until("the question", func(ev *relayv1.Envelope) bool { return ev.GetQuestion() != nil })
	e.relay.cancel("a/t1")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "cancelled" {
		t.Errorf("t1 ended with %q, want cancelled", err)
	}
}

// TestSendMessage: send_message is a Message to the lead with the task.
func TestSendMessage(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "message halfway there")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	var m *relayv1.Message
	for _, ev := range got {
		if ev.GetMessage() != nil {
			m = ev.GetMessage()
		}
	}
	if m.GetTo() != "lead" || m.GetText() != "halfway there" || m.GetTaskId() != "a/t1" {
		t.Errorf("message %v", m)
	}
}

// TestFail: a turn whose claude fails ends with its exit and its stderr.
func TestFail(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "fail")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	err := got[len(got)-1].GetTurnEvent().GetError()
	if !strings.Contains(err, "exit status 3") || !strings.Contains(err, "something broke") {
		t.Errorf("t1 ended with %q, want the exit status and the stderr", err)
	}
}

// TestResumeGone: a session claude does not have is forgotten, and the
// turn runs in a new one.
func TestResumeGone(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.StateDir, "session"), []byte(goneSession+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.relay.task("a/t1", "echo hi")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "" {
		t.Errorf("t1 ended with %q", err)
	}
	runs := e.runs()
	if len(runs) != 2 || resumeOf(runs[0]) != goneSession || resumeOf(runs[1]) != "" {
		t.Errorf("runs %q, want one resuming gone, then one new", runs)
	}
	raw, _ := os.ReadFile(filepath.Join(e.cfg.StateDir, "session"))
	if strings.TrimSpace(string(raw)) != newSession {
		t.Errorf("session %q after the retry, want the new one", raw)
	}
}

// TestReconnect: a stream that breaks in the middle of a turn loses
// nothing: the shim reconnects, resends what was not acked, and the relay
// takes each envelope once.
func TestReconnect(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "slow 8")
	e.relay.until("the first line", func(ev *relayv1.Envelope) bool {
		return strings.Contains(string(ev.GetTurnEvent().GetChunk()), "line 0")
	})
	e.relay.stop()
	time.Sleep(300 * time.Millisecond)
	e.relay.start()
	got := e.relay.until("t1 done", doneOf("a/t1"))
	seen := map[string]int{}
	for _, c := range chunks(got) {
		seen[c]++
	}
	for i := 1; i < 8; i++ {
		n := 0
		for c, k := range seen {
			if strings.Contains(c, fmt.Sprintf(`"line %d"`, i)) {
				n += k
			}
		}
		if n != 1 {
			t.Errorf("line %d arrived %d times, want once", i, n)
		}
	}
}

// TestRestartReportsInterrupted: a turn running when the shim stopped is
// reported interrupted at the next start, and the queued tasks run.
func TestRestartReportsInterrupted(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "sleep")
	e.relay.task("a/t2", "echo later")
	e.relay.until("t1's init", func(ev *relayv1.Envelope) bool { return len(ev.GetTurnEvent().GetChunk()) > 0 })
	e.stop()

	e.start()
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); !strings.Contains(err, "interrupted") {
		t.Errorf("t1 ended with %q, want interrupted", err)
	}
	e.relay.until("t2 done", doneOf("a/t2"))
}

// childPid waits for fakeclaude's child to write its pid.
func (e *env) childPid() int {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(e.pids); err == nil && len(b) > 0 {
			pid, err := strconv.Atoi(string(b))
			if err != nil {
				e.t.Fatal(err)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("fakeclaude's child wrote no pid")
	return 0
}

// gone waits until pid is no process at all: not alive, and not a zombie
// either (reaped).
func gone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still there, alive or a zombie", what, pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCancelKillsDescendants: a cancel ends claude's descendants too, one
// in a session of its own (setsid) that ignores SIGTERM included, and the
// dead are reaped. The turn is the shim's whole subtree (reaper.Kill).
func TestCancelKillsDescendants(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "spawn")
	pid := e.childPid()
	e.relay.cancel("a/t1")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "cancelled" {
		t.Errorf("t1 ended with %q", err)
	}
	gone(t, pid, "the cancelled turn's setsid child")
}

// TestLeftoversEndWithTheTurn: what claude leaves running when it exits
// goes with its turn, and is reaped.
func TestLeftoversEndWithTheTurn(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "leave")
	pid := e.childPid()
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "" {
		t.Errorf("t1 ended with %q", err)
	}
	gone(t, pid, "the turn's leftover child")
}

// TestEndedTaskNotRunAgain: a task delivered again after it ended (its ack
// lost to a stop) is not run again.
func TestEndedTaskNotRunAgain(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "echo once")
	e.relay.until("t1 done", doneOf("a/t1"))
	e.stop()
	// A restarted shim, with the task delivered again.
	e.start()
	e.relay.task("a/t1", "echo once")
	e.relay.task("a/t2", "echo next")
	got := e.relay.until("t2 done", doneOf("a/t2"))
	for _, ev := range got {
		if ev.GetTurnEvent().GetTaskId() == "a/t1" {
			t.Errorf("t1 ran again: %v", ev)
		}
	}
	if n := len(e.runs()); n != 2 {
		t.Errorf("%d runs, want t1's and t2's", n)
	}
}

// TestSessionFileMustBeASession: a session file that holds anything but a
// session id (the turn can write it) is never passed to claude, not even
// as a value: it is dropped and a new session starts.
func TestSessionFileMustBeASession(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.StateDir, "session"), []byte("--permission-mode=bypassPermissions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.relay.task("a/t1", "echo hi")
	e.relay.until("t1 done", doneOf("a/t1"))
	runs := e.runs()
	if len(runs) != 1 || resumeOf(runs[0]) != "" || strings.Contains(strings.Join(runs[0], " "), "bypassPermissions") {
		t.Errorf("runs %q, want one without a resume", runs)
	}
	raw, _ := os.ReadFile(filepath.Join(e.cfg.StateDir, "session"))
	if strings.TrimSpace(string(raw)) != newSession {
		t.Errorf("session %q, want the new one", raw)
	}
}

// TestCrashKeepsTheSession: a turn that fails at once for another reason
// than a missing session fails, and the session is kept.
func TestCrashKeepsTheSession(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.StateDir, "session"), []byte(newSession+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.relay.task("a/t1", "crash")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); !strings.Contains(err, "exit status 137") {
		t.Errorf("t1 ended with %q, want the crash", err)
	}
	if n := len(e.runs()); n != 1 {
		t.Errorf("%d runs, want no retry", n)
	}
	raw, _ := os.ReadFile(filepath.Join(e.cfg.StateDir, "session"))
	if strings.TrimSpace(string(raw)) != newSession {
		t.Errorf("session %q after a crash, want it kept", raw)
	}
}

// TestLongLine: a line over maxChunk goes in parts, every one but the last
// partial, which put together are the line.
func TestLongLine(t *testing.T) {
	e := newEnv(t)
	e.start()
	text := strings.Repeat("y", 2*maxChunk+100)
	e.relay.task("a/t1", "echo "+text)
	got := e.relay.until("t1 done", doneOf("a/t1"))
	var line []byte
	var parts int
	for _, ev := range got {
		te := ev.GetTurnEvent()
		if parts == 0 && !strings.Contains(string(te.GetChunk()), "yyyyyyyy") {
			continue
		}
		if len(te.GetChunk()) > maxChunk {
			t.Fatalf("a chunk of %d bytes", len(te.GetChunk()))
		}
		line = append(line, te.GetChunk()...)
		parts++
		if !te.GetPartial() {
			break
		}
	}
	if parts != 3 || !strings.Contains(string(line), text) {
		t.Errorf("%d parts, %d bytes; want the line in 3", parts, len(line))
	}
}

// TestMessageNeedsATurn: send_message outside a turn has no task to name.
func TestMessageNeedsATurn(t *testing.T) {
	e := newEnv(t)
	s := e.start()
	if err := s.Message(context.Background(), "hi"); err == nil {
		t.Error("a message with no turn running: accepted")
	}
}

// TestRelayLimits: what the relay would drop is not sent, at the exact
// boundaries: a done error up to 4096 bytes as it is and cut beyond, never
// in the middle of a rune; invalid UTF-8 made valid, so the done is sent;
// a question or message up to 16384 bytes, refused to the tool beyond.
func TestRelayLimits(t *testing.T) {
	errOf := func(msg string) string { return done("a/t1", errors.New(msg)).GetTurnEvent().GetError() }
	if got := errOf(strings.Repeat("e", maxError)); len(got) != maxError || strings.HasSuffix(got, "[cut]") {
		t.Errorf("a %d-byte error: %d bytes, cut %v; want it whole", maxError, len(got), strings.HasSuffix(got, "[cut]"))
	}
	if got := errOf(strings.Repeat("e", maxError+1)); len(got) > maxError || !strings.HasSuffix(got, "[cut]") {
		t.Errorf("a %d-byte error: %d bytes; want cut to %d", maxError+1, len(got), maxError)
	}
	// A 4-byte rune across the cut, wherever it falls.
	for pad := range 4 {
		got := errOf(strings.Repeat("e", maxError-len(" [cut]")-pad) + strings.Repeat("😀", 4))
		if !utf8.ValidString(got) || len(got) > maxError {
			t.Errorf("a rune across the cut (pad %d): valid %v, %d bytes", pad, utf8.ValidString(got), len(got))
		}
	}
	if got := errOf("bad \xff\xfe \xf0\x9f\x98"); !utf8.ValidString(got) || !strings.HasPrefix(got, "bad ") {
		t.Errorf("an invalid error: %q", got)
	}
	if _, err := proto.Marshal(done("a/t1", errors.New("\xff\xfe"))); err != nil {
		t.Errorf("a done with an invalid error does not marshal: %v", err)
	}

	e := newEnv(t)
	s := e.start()
	// A turn, so that the tools have a task.
	e.relay.task("a/t1", "ask please")
	e.relay.until("the question", func(ev *relayv1.Envelope) bool { return ev.GetQuestion() != nil })
	for _, n := range []int{maxText, maxText + 1} {
		err := s.Message(context.Background(), strings.Repeat("m", n))
		if (n <= maxText) != (err == nil) {
			t.Errorf("a %d-byte message: %v", n, err)
		}
		if n > maxText && !strings.Contains(err.Error(), "shorter") {
			t.Errorf("a %d-byte message: %v, want to be told to say it shorter", n, err)
		}
	}
	got := e.relay.until("the message", func(ev *relayv1.Envelope) bool { return ev.GetMessage() != nil })
	if m := got[len(got)-1].GetMessage(); len(m.GetText()) != maxText {
		t.Errorf("the %d-byte message arrived with %d bytes", maxText, len(m.GetText()))
	}
	if _, err := s.Ask(context.Background(), strings.Repeat("q", maxText+1)); err == nil || !strings.Contains(err.Error(), "shorter") {
		t.Errorf("a %d-byte question: %v", maxText+1, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Ask(ctx, strings.Repeat("q", maxText)); err == nil || strings.Contains(err.Error(), "shorter") {
		t.Errorf("a %d-byte question: %v, want it sent (and this ask cancelled)", maxText, err)
	}
	e.relay.until("the long question", func(ev *relayv1.Envelope) bool { return len(ev.GetQuestion().GetText()) == maxText })
	e.relay.cancel("a/t1")
	e.relay.until("t1 done", doneOf("a/t1"))
}

// TestInvalidStderr: a claude whose standard error is not UTF-8 still ends
// its turn on the relay, with a valid error.
func TestInvalidStderr(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.relay.task("a/t1", "badfail")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); !utf8.ValidString(err) || !strings.Contains(err, "exit status 3") {
		t.Errorf("t1 ended with %q", err)
	}
}

// TestChunkBoundary: a line of exactly maxChunk bytes is one event; one byte
// more is two, the first partial.
func TestChunkBoundary(t *testing.T) {
	e := newEnv(t)
	e.start()
	// echo wraps the text in a JSON line of this many bytes besides it.
	const wrap = len(`{"message":{"content":[{"text":"","type":"text"}]},"type":"assistant"}`)
	for i, n := range []int{maxChunk, maxChunk + 1} {
		task := fmt.Sprintf("a/t%d", i)
		e.relay.task(task, "echo "+strings.Repeat("z", n-wrap))
		got := e.relay.until(task+" done", doneOf(task))
		var parts []*relayv1.TurnEvent
		for _, ev := range got {
			if te := ev.GetTurnEvent(); strings.Contains(string(te.GetChunk()), "zzzz") || (len(parts) > 0 && len(te.GetChunk()) > 0 && parts[len(parts)-1].GetPartial()) {
				parts = append(parts, te)
			}
		}
		total := 0
		for _, p := range parts {
			total += len(p.GetChunk())
		}
		wantParts := 1 + (n-1)/maxChunk
		if len(parts) != wantParts || total != n || parts[len(parts)-1].GetPartial() || (wantParts > 1 && !parts[0].GetPartial()) {
			t.Errorf("a %d-byte line: %d parts, %d bytes; want %d parts, the last whole", n, len(parts), total, wantParts)
		}
	}
}

// TestSessionNotFoundWording pins what claude says of a session it does not
// have: the session is forgotten only on it, so a claude upgrade that
// rewords it must change sessionNotFound (and fakeclaude) with it.
func TestSessionNotFoundWording(t *testing.T) {
	if sessionNotFound != "No conversation found" {
		t.Errorf("sessionNotFound is %q: recheck it against the claude in the image", sessionNotFound)
	}
}

// stuckReaper is the real reaper whose Kill, while stuck, reports a
// survivor, as one in the kernel's D state would leave.
type stuckReaper struct {
	*reaper.Reaper
	stuck atomic.Bool
}

func (r *stuckReaper) Kill(grace time.Duration) error {
	err := r.Reaper.Kill(grace)
	if r.stuck.Load() {
		return errors.New("processes of the turn survive SIGKILL: [4242]")
	}
	return err
}

// TestSurvivorDoesNotWedge: with a survivor held, the turn's end still
// arrives (naming it), the queued task is told why it waits and can be
// cancelled, and nothing starts until the survivor is gone.
func TestSurvivorDoesNotWedge(t *testing.T) {
	e := newEnv(t)
	sr := &stuckReaper{Reaper: e.cfg.Reaper.(*reaper.Reaper)}
	sr.stuck.Store(true)
	e.cfg.Reaper = sr
	e.cfg.SettleEvery = 10 * time.Millisecond
	e.start()
	e.relay.task("a/t1", "echo hi")
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); !strings.Contains(err, "survive SIGKILL") {
		t.Errorf("t1 ended with %q, want the survivors named", err)
	}
	e.relay.task("a/t2", "echo later")
	got = e.relay.until("t2's wait", func(ev *relayv1.Envelope) bool {
		return ev.GetTurnEvent().GetTaskId() == "a/t2" && strings.Contains(string(ev.GetTurnEvent().GetChunk()), `"waiting"`)
	})
	if te := got[len(got)-1].GetTurnEvent(); te.GetDone() || !strings.Contains(string(te.GetChunk()), "4242") {
		t.Errorf("the wait: %v", te)
	}
	e.relay.cancel("a/t2")
	got = e.relay.until("t2 done", doneOf("a/t2"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "cancelled" {
		t.Errorf("t2 ended with %q, want cancelled", err)
	}
	e.relay.task("a/t3", "echo free")
	select {
	case ev := <-e.relay.got:
		if len(ev.GetTurnEvent().GetChunk()) > 0 && !strings.Contains(string(ev.GetTurnEvent().GetChunk()), `"waiting"`) {
			t.Fatalf("t3 started beside a survivor: %v", ev)
		}
	case <-time.After(200 * time.Millisecond):
	}
	sr.stuck.Store(false)
	got = e.relay.until("t3 done", doneOf("a/t3"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "" {
		t.Errorf("t3 ended with %q", err)
	}
	if n := len(e.runs()); n != 2 {
		t.Errorf("%d runs, want t1's and t3's", n)
	}
}

// holdingReaper is the real reaper whose Kill, while held, kills nothing
// and reports a survivor: the turn's leftovers really stay, holding
// claude's pipes.
type holdingReaper struct {
	*reaper.Reaper
	held atomic.Bool
}

func (r *holdingReaper) Kill(grace time.Duration) error {
	if r.held.Load() {
		return errors.New("processes of the turn survive SIGKILL: [held]")
	}
	return r.Reaper.Kill(grace)
}

// waits are the waiting events of task among es.
func waits(es []*relayv1.Envelope, task string) int {
	n := 0
	for _, ev := range es {
		if ev.GetTurnEvent().GetTaskId() == task && strings.Contains(string(ev.GetTurnEvent().GetChunk()), `"waiting"`) {
			n++
		}
	}
	return n
}

// TestRealSurvivorHoldingPipes: a leftover that truly survives and holds
// claude's output and error pipes neither hangs the turn (its end
// arrives, naming it) nor lets the next one start; the waiting task is
// told again and again; once the survivor goes, the next turn runs.
func TestRealSurvivorHoldingPipes(t *testing.T) {
	e := newEnv(t)
	hr := &holdingReaper{Reaper: e.cfg.Reaper.(*reaper.Reaper)}
	hr.held.Store(true)
	e.cfg.Reaper = hr
	e.cfg.SettleEvery = 10 * time.Millisecond
	e.cfg.WaitNoticeEvery = 30 * time.Millisecond
	e.start()
	e.relay.task("a/t1", "leave")
	pid := e.childPid()
	got := e.relay.until("t1 done", doneOf("a/t1"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); !strings.Contains(err, "survive SIGKILL") {
		t.Errorf("t1 ended with %q, want the survivor named", err)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the survivor is gone: the test holds nothing")
	}
	e.relay.task("a/t2", "echo later")
	n := 0
	e.relay.until("t2 told three times", func(ev *relayv1.Envelope) bool {
		n += waits([]*relayv1.Envelope{ev}, "a/t2")
		if ev.GetTurnEvent().GetTaskId() == "a/t2" && !strings.Contains(string(ev.GetTurnEvent().GetChunk()), `"waiting"`) {
			t.Fatalf("t2 started beside the survivor: %v", ev)
		}
		return n >= 3
	})
	hr.held.Store(false)
	got = e.relay.until("t2 done", doneOf("a/t2"))
	if err := got[len(got)-1].GetTurnEvent().GetError(); err != "" {
		t.Errorf("t2 ended with %q", err)
	}
	gone(t, pid, "the survivor, once let go")
}

// TestNewHeadToldAtOnce: when the lead cancels the task that waits, the
// next one is told at once, not after WaitNoticeEvery.
func TestNewHeadToldAtOnce(t *testing.T) {
	e := newEnv(t)
	sr := &stuckReaper{Reaper: e.cfg.Reaper.(*reaper.Reaper)}
	sr.stuck.Store(true)
	e.cfg.Reaper = sr
	e.cfg.SettleEvery = time.Hour
	e.cfg.WaitNoticeEvery = time.Hour
	e.start()
	e.relay.task("a/t1", "echo hi")
	e.relay.until("t1 done", doneOf("a/t1"))
	e.relay.task("a/t2", "echo two")
	e.relay.until("t2's wait", func(ev *relayv1.Envelope) bool { return waits([]*relayv1.Envelope{ev}, "a/t2") > 0 })
	e.relay.task("a/t3", "echo three")
	e.relay.cancel("a/t2")
	e.relay.until("t3's wait", func(ev *relayv1.Envelope) bool { return waits([]*relayv1.Envelope{ev}, "a/t3") > 0 })
	sr.stuck.Store(false)
	e.relay.cancel("a/t3")
	e.relay.until("t3 done", doneOf("a/t3"))
}

// slowReaper reports a survivor after a slow kill pass, as a SIGTERM grace
// would make it.
type slowReaper struct {
	*reaper.Reaper
	stuck atomic.Bool
}

func (r *slowReaper) Kill(grace time.Duration) error {
	err := r.Reaper.Kill(grace)
	if r.stuck.Load() {
		time.Sleep(700 * time.Millisecond)
		return errors.New("processes of the turn survive SIGKILL: [4242]")
	}
	return err
}

// TestNewHeadToldBeforeTheKill: a new head is told as soon as the old is
// cancelled, not after the next kill pass.
func TestNewHeadToldBeforeTheKill(t *testing.T) {
	e := newEnv(t)
	sr := &slowReaper{Reaper: e.cfg.Reaper.(*reaper.Reaper)}
	sr.stuck.Store(true)
	e.cfg.Reaper = sr
	e.cfg.SettleEvery = time.Hour
	e.cfg.WaitNoticeEvery = time.Hour
	e.start()
	e.relay.task("a/t1", "echo hi")
	e.relay.task("a/t2", "echo two")
	e.relay.task("a/t3", "echo three")
	e.relay.until("t2's wait", func(ev *relayv1.Envelope) bool { return waits([]*relayv1.Envelope{ev}, "a/t2") > 0 })
	start := time.Now()
	e.relay.cancel("a/t2")
	e.relay.until("t3's wait", func(ev *relayv1.Envelope) bool { return waits([]*relayv1.Envelope{ev}, "a/t3") > 0 })
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("the new head was told after %v, after a kill pass", d)
	}
	sr.stuck.Store(false)
	e.relay.cancel("a/t3")
	e.relay.until("t3 done", doneOf("a/t3"))
}
