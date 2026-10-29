// Package shim is redoma-shim's daemon, in an agent's container: it holds
// the agent's one stream to redoma-relay, runs the lead's tasks one at a
// time as claude turns, and streams their output back.
//
// State, in StateDir (the workspace's .redoma, which outlives the
// container): the queue to the relay and the record of what came from it
// (common/reliable), the tasks received and not yet run (tasks.json, saved
// before they are acked, so a task acked is never lost), and the claude
// session the turns resume (session).
package shim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/metadata"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
	"github.com/bpalermo/redoma/common/atomicfile"
	"github.com/bpalermo/redoma/common/reliable"
	"github.com/bpalermo/redoma/shim/internal/bridge"
	"github.com/bpalermo/redoma/shim/internal/reaper"
)

// Config is what the shim needs.
type Config struct {
	// StateDir holds the shim's state (/work/.redoma).
	StateDir string
	// Workdir is where claude runs (/work).
	Workdir string
	// Claude is the claude binary, ClaudeArgs more arguments for every turn.
	Claude     string
	ClaudeArgs []string
	// Self is this binary, which claude runs as its MCP server
	// (redoma-shim mcp).
	Self string
	// Relay is the relay's client, through the Envoy listener.
	Relay relayv1.RelayClient
	// Bridge is where the MCP server reaches the shim.
	Bridge net.Listener
	// AckEvery and HeartbeatEvery are the stream's (reliable.Session).
	AckEvery       time.Duration
	HeartbeatEvery time.Duration
	// MinBackoff and MaxBackoff bound the wait between reconnects.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// KillGrace is how long a cancelled turn has between SIGTERM and SIGKILL.
	KillGrace time.Duration
	// Reaper starts claude, reaps what the turns leave, and kills a turn
	// (reaper.Reaper; a test's own).
	Reaper Reaper
	// SettleEvery is how often survivors of a turn are killed again before
	// the next turn may start; 0 is 5s. WaitNoticeEvery is how often the
	// task that waits for them is told again; 0 is a minute.
	SettleEvery     time.Duration
	WaitNoticeEvery time.Duration
	Log             *slog.Logger
}

// Reaper is what the shim needs of reaper.Reaper.
type Reaper interface {
	Start(cmd *exec.Cmd) error
	Done(pid int)
	Kill(grace time.Duration) error
}

var _ Reaper = (*reaper.Reaper)(nil)

// Shim runs. Make it with New.
type Shim struct {
	cfg Config
	out *reliable.Outbox
	in  *reliable.Inbox
	log *slog.Logger

	mu        sync.Mutex
	tasks     taskState
	cur       *turn
	questions map[string]chan string
	wake      chan struct{}
	// headChanged wakes settle when the queue's head may have changed.
	headChanged chan struct{}
}

// taskState is tasks.json.
type taskState struct {
	// Queue are the tasks received and not started, in order.
	Queue []task `json:"queue,omitempty"`
	// Running is the task whose turn was running, if any: found at start,
	// that turn was interrupted.
	Running string `json:"running,omitempty"`
	// Ended are the tasks that ran, were cancelled or were interrupted,
	// the last maxEnded: one the relay delivers again (its ack lost to a
	// stop) is not run again.
	Ended []string `json:"ended,omitempty"`
}

// maxEnded is how many ended tasks are remembered: far more than the relay
// lets the lead have open with an agent.
const maxEnded = 1024

// end records a task as ended; s.mu is held (or nothing else runs yet).
func (s *Shim) end(id string) {
	s.tasks.Ended = append(s.tasks.Ended, id)
	if n := len(s.tasks.Ended) - maxEnded; n > 0 {
		s.tasks.Ended = s.tasks.Ended[n:]
	}
}

type task struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// turn is the running task's: its context ends with the turn, its cause
// errCancelled for a Cancel.
type turn struct {
	id     string
	text   string
	ctx    context.Context
	cancel context.CancelCauseFunc
}

var (
	errCancelled   = errors.New("cancelled")
	errInterrupted = errors.New("interrupted: the shim stopped during the turn")
)

// New opens the shim's state. A turn found running (the shim stopped in
// the middle of it) is reported as interrupted, not run again: what it had
// done is in the workspace, and the lead decides.
func New(cfg Config) (*Shim, error) {
	out, err := reliable.OpenOutbox(filepath.Join(cfg.StateDir, "out"), false)
	if err != nil {
		return nil, err
	}
	in, err := reliable.OpenInbox(filepath.Join(cfg.StateDir, "in.json"))
	if err != nil {
		out.Close()
		return nil, err
	}
	if cfg.SettleEvery == 0 {
		cfg.SettleEvery = 5 * time.Second
	}
	if cfg.WaitNoticeEvery == 0 {
		cfg.WaitNoticeEvery = time.Minute
	}
	s := &Shim{cfg: cfg, out: out, in: in, log: cfg.Log, questions: map[string]chan string{}, wake: make(chan struct{}, 1), headChanged: make(chan struct{}, 1)}
	raw, err := os.ReadFile(s.path("tasks.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		out.Close()
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.tasks); err != nil {
			out.Close()
			return nil, fmt.Errorf("%s: %w", s.path("tasks.json"), err)
		}
	}
	if id := s.tasks.Running; id != "" {
		s.log.Warn("a turn was interrupted", "task", id)
		s.pushDone(id, errInterrupted)
		s.tasks.Running = ""
		s.end(id)
		s.saveTasks()
	}
	return s, nil
}

func (s *Shim) path(name string) string { return filepath.Join(s.cfg.StateDir, name) }

// Close closes the queue.
func (s *Shim) Close() { s.out.Close() }

// Run runs the shim until ctx ends.
func (s *Shim) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := bridge.Serve(ctx, s.cfg.Bridge, s); err != nil {
			s.log.Error("the MCP bridge stopped", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		s.work(ctx)
	}()
	s.connect(ctx)
	cancel()
	wg.Wait()
	// What was taken is saved, so a restart does not take it again.
	if _, _, err := s.in.Flush(); err != nil {
		s.log.Error("could not save what the relay sent", "err", err)
	}
	return nil
}

// saveTasks writes tasks.json; s.mu is held (or nothing else runs yet).
func (s *Shim) saveTasks() {
	raw, err := json.Marshal(s.tasks)
	if err == nil {
		err = atomicfile.Write(s.path("tasks.json"), raw, 0o600)
	}
	if err != nil {
		s.log.Error("could not save the tasks; a restart would lose them", "err", err)
	}
}

// push queues an envelope to the relay.
func (s *Shim) push(e *relayv1.Envelope) {
	if err := s.out.Push(e); err != nil {
		s.log.Error("could not queue an envelope for the relay", "err", err)
	}
}

// errUnsendable is a done's error when the done itself could not be queued:
// a task must end on the relay, or it holds the lead's quota for good.
const errUnsendable = "the turn ended, but its end could not be sent as it was"

// pushDone queues a task's end; one that cannot be queued is queued again
// with a fixed error.
func (s *Shim) pushDone(id string, err error) {
	if perr := s.out.Push(done(id, err)); perr != nil {
		s.log.Error("could not queue a turn's end; sending it with a fixed error", "task", id, "err", perr)
		s.push(done(id, errors.New(errUnsendable)))
	}
}

// The relay's limits on what an agent sends (redoma-relay's hub): a done
// event's error is cut to fit, since a task must be able to end; a longer
// question or message is refused to the tool, which can say it shorter.
const (
	maxError = 4 << 10
	maxText  = 16 << 10
)

func done(id string, err error) *relayv1.Envelope {
	ev := relayv1.TurnEvent_builder{TaskId: id, Done: true}.Build()
	if err != nil {
		ev.SetError(text(err.Error(), maxError))
	}
	return relayv1.Envelope_builder{TurnEvent: ev}.Build()
}

// connect holds the stream to the relay, reconnecting with a jittered,
// doubling backoff, until ctx ends.
func (s *Shim) connect(ctx context.Context) {
	backoff := s.cfg.MinBackoff
	for {
		start := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		// A stream that lived a while was a good one: start over.
		if time.Since(start) > 30*time.Second {
			backoff = s.cfg.MinBackoff
		}
		s.log.Warn("the stream to the relay ended; reconnecting", "err", err, "after", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff/2 + rand.N(backoff/2+1)):
		}
		backoff = min(2*backoff, s.cfg.MaxBackoff)
	}
}

// session runs one stream.
func (s *Shim) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := s.cfg.Relay.Connect(metadata.AppendToOutgoingContext(ctx, reliable.EpochKey, s.out.Epoch()))
	if err != nil {
		return err
	}
	md, err := st.Header()
	if err != nil {
		return err
	}
	v := md.Get(reliable.EpochKey)
	if len(v) != 1 || v[0] == "" {
		return fmt.Errorf("the relay sent no %s", reliable.EpochKey)
	}
	s.log.Info("connected to the relay", "pending", s.out.Len())
	sess := reliable.Session{
		Out: s.out, In: s.in, PeerEpoch: v[0], Handle: s.handle,
		AckEvery: s.cfg.AckEvery, HeartbeatEvery: s.cfg.HeartbeatEvery,
	}
	return sess.Run(ctx, st)
}

// handle takes one envelope from the relay. A task is saved before handle
// returns, so before it is acked.
func (s *Shim) handle(e *relayv1.Envelope) {
	switch e.WhichBody() {
	case relayv1.Envelope_Task_case:
		s.queue(task{ID: e.GetTask().GetId(), Text: e.GetTask().GetText()})
	case relayv1.Envelope_Reply_case:
		s.mu.Lock()
		ch, ok := s.questions[e.GetReply().GetQuestionId()]
		delete(s.questions, e.GetReply().GetQuestionId())
		s.mu.Unlock()
		if !ok {
			s.log.Warn("a reply to no question waiting (its turn ended?)", "question", e.GetReply().GetQuestionId())
			return
		}
		ch <- e.GetReply().GetText()
	case relayv1.Envelope_Cancel_case:
		s.cancel(e.GetCancel().GetTaskId())
	default:
		s.log.Warn("dropping an envelope the relay does not send", "type", fmt.Sprintf("body field %d", e.WhichBody()))
	}
}

func (s *Shim) queue(t task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == s.tasks.Running || slices.Contains(s.tasks.Ended, t.ID) || slices.ContainsFunc(s.tasks.Queue, func(q task) bool { return q.ID == t.ID }) {
		s.log.Info("a task delivered again; already queued, running or ended", "task", t.ID)
		return
	}
	s.tasks.Queue = append(s.tasks.Queue, t)
	s.saveTasks()
	s.log.Info("task queued", "task", t.ID, "queued", len(s.tasks.Queue))
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.changedHead()
}

// cancel stops the task: its running turn is killed; queued, it is dropped
// and reported done.
func (s *Shim) cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.id == id {
		s.log.Info("cancelling the running turn", "task", id)
		s.cur.cancel(errCancelled)
		return
	}
	i := slices.IndexFunc(s.tasks.Queue, func(q task) bool { return q.ID == id })
	if i < 0 {
		s.log.Info("a cancel for a task neither running nor queued", "task", id)
		return
	}
	s.tasks.Queue = slices.Delete(s.tasks.Queue, i, i+1)
	s.end(id)
	s.saveTasks()
	s.changedHead()
	s.pushDone(id, errCancelled)
}

// work runs the queued tasks, one turn at a time, until ctx ends.
func (s *Shim) work(ctx context.Context) {
	for {
		t, ok := s.next(ctx)
		if !ok {
			return
		}
		err := s.runTask(t)
		t.cancel(nil)
		if ctx.Err() != nil {
			// Stopping: the turn was killed, and is reported interrupted
			// at the next start (Running stays saved).
			return
		}
		// The turn's end goes first, its error naming any survivor: the
		// task closes on the relay and a Cancel is answered whatever
		// the survivors do.
		s.pushDone(t.id, err)
		s.mu.Lock()
		s.cur = nil
		s.tasks.Running = ""
		s.end(t.id)
		s.saveTasks()
		s.mu.Unlock()
		s.log.Info("turn done", "task", t.id, "err", err)
		// Nothing of this turn may run beside the next: it could write
		// the workspace or ask the lead under the next task's id.
		s.settle(ctx)
	}
}

// settle waits until nothing is left below the shim, killing it again
// every SettleEvery, for as long as it takes or until ctx ends. While it
// waits, the task next in the queue is told why it does not start (a
// TurnEvent, not its last, naming the survivors), at the first failure and
// every waitNoticeEvery after, so the lead sees it and can cancel; queued
// tasks are cancelled as ever.
func (s *Shim) settle(ctx context.Context) {
	var told time.Time
	var toldID string
	tell := func(err error) {
		if head := s.head(); head != "" && (head != toldID || time.Since(told) >= s.cfg.WaitNoticeEvery) {
			s.tellWaiting(head, err)
			told, toldID = time.Now(), head
		}
	}
	for {
		err := s.cfg.Reaper.Kill(s.cfg.KillGrace)
		if err == nil {
			return
		}
		s.log.Error("the last turn's processes survive; no turn starts until they are gone", "err", err)
		// The head is told when it changes (a new task, or the old head
		// cancelled), and again every WaitNoticeEvery.
		tell(err)
		if !s.waitSettle(ctx, func() { tell(err) }) {
			return
		}
	}
}

// waitSettle waits SettleEvery before the next kill pass; a change of the
// queue's head meanwhile is told at once (onHead), of what the last pass
// found, and the wait goes on. False when ctx ends.
func (s *Shim) waitSettle(ctx context.Context, onHead func()) bool {
	timer := time.NewTimer(s.cfg.SettleEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.headChanged:
			onHead()
		case <-timer.C:
			return true
		}
	}
}

// head is the task next in the queue, if any.
func (s *Shim) head() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks.Queue) == 0 {
		return ""
	}
	return s.tasks.Queue[0].ID
}

// changedHead wakes settle: the queue's head may be another.
func (s *Shim) changedHead() {
	select {
	case s.headChanged <- struct{}{}:
	default:
	}
}

// tellWaiting tells the task id, next in the queue, that it waits for the
// last turn's survivors.
func (s *Shim) tellWaiting(id string, err error) {
	line, _ := json.Marshal(map[string]string{
		"type": "redoma_shim", "subtype": "waiting",
		"error": text("the last turn's processes survive SIGKILL, and this task waits for them: "+err.Error(), maxText),
	})
	s.push(turnEvent(id, line, false))
}

// next takes the next task, waiting for one, and marks it running.
func (s *Shim) next(ctx context.Context) (*turn, bool) {
	for {
		s.mu.Lock()
		if len(s.tasks.Queue) > 0 {
			t := s.tasks.Queue[0]
			s.tasks.Queue = s.tasks.Queue[1:]
			s.tasks.Running = t.ID
			s.saveTasks()
			tctx, cancel := context.WithCancelCause(ctx)
			s.cur = &turn{id: t.ID, text: t.Text, ctx: tctx, cancel: cancel}
			cur := s.cur
			s.mu.Unlock()
			return cur, true
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-s.wake:
		}
	}
}

// Ask is ask_lead: a Question to the lead, answered by its Reply. It waits
// until the reply, ctx's end, or the turn's.
func (s *Shim) Ask(ctx context.Context, question string) (string, error) {
	if len(question) > maxText {
		return "", fmt.Errorf("a question of %d bytes is over the relay's %d; ask it shorter", len(question), maxText)
	}
	s.mu.Lock()
	cur := s.cur
	if cur == nil {
		s.mu.Unlock()
		return "", errors.New("no turn is running")
	}
	id := newQuestionID()
	ch := make(chan string, 1)
	s.questions[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.questions, id)
		s.mu.Unlock()
	}()
	s.push(relayv1.Envelope_builder{Question: relayv1.Question_builder{Id: id, Text: text(question, maxText), TaskId: cur.id}.Build()}.Build())
	s.log.Info("asked the lead", "task", cur.id, "question", id)
	select {
	case text := <-ch:
		return text, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-cur.ctx.Done():
		return "", context.Cause(cur.ctx)
	}
}

// Message is send_message: of the running turn's task (the relay takes no
// message without one).
func (s *Shim) Message(_ context.Context, msg string) error {
	if len(msg) > maxText {
		return fmt.Errorf("a message of %d bytes is over the relay's %d; say it shorter", len(msg), maxText)
	}
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur == nil {
		return errors.New("no turn is running")
	}
	id := cur.id
	s.push(relayv1.Envelope_builder{Message: relayv1.Message_builder{To: "lead", Text: text(msg, maxText), TaskId: id}.Build()}.Build())
	return nil
}

var _ bridge.Lead = (*Shim)(nil)

func newQuestionID() string {
	return fmt.Sprintf("q-%016x", rand.Uint64())
}

// text is s as a proto string field holds it: valid UTF-8 (anything else
// fails the marshal, and the envelope with it), then cut to n bytes.
func text(s string, n int) string {
	return truncate(strings.ToValidUTF8(s, "�"), n)
}

// truncate cuts s to at most n bytes, on a rune boundary, marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = " [cut]"
	cut := n - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}
