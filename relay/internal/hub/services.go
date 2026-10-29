package hub

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/common/allowlist"
	"github.com/bpalermo/redoma/common/reliable"
	"github.com/bpalermo/redoma/relay/internal/mtls"
)

// maxText bounds a task's or a reply's text.
const maxText = 1 << 20

// RelayServer is the agents' service; serve it with mtls.ServerConfig.
func (h *Hub) RelayServer() relayv1.RelayServer { return relayService{h: h} }

// LeadServer is the lead's service.
func (h *Hub) LeadServer() relayv1.LeadServer { return leadService{h: h} }

type relayService struct {
	relayv1.UnimplementedRelayServer
	h *Hub
}

// peerEpoch is the agent's queue epoch from the stream's metadata.
func peerEpoch(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get(reliable.EpochKey)
	if len(v) != 1 || v[0] == "" || len(v[0]) > 64 {
		return "", status.Errorf(codes.InvalidArgument, "one %s of 1 to 64 bytes is required", reliable.EpochKey)
	}
	return v[0], nil
}

// Connect is an agent's stream. The agent is the one its connection's
// client certificate names, and nothing the stream says.
func (s relayService) Connect(st relayv1.Relay_ConnectServer) error {
	h := s.h
	h.wg.Add(1)
	defer h.wg.Done()
	name, err := mtls.PeerAgent(st.Context(), h.trustDomain)
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	// The handshake checked the allowlist; again here, for a stream on a
	// connection made before the name left it.
	if ok, err := h.allowed(name); err != nil || !ok {
		return status.Errorf(codes.PermissionDenied, "agent %q is not allowed", name)
	}
	epoch, err := peerEpoch(st.Context())
	if err != nil {
		return err
	}
	a, err := h.agent(name)
	if err != nil {
		return status.Error(codes.Unavailable, "the relay's state of this agent is broken; the agent is quarantined")
	}
	if a.journalBroken() {
		return status.Error(codes.Unavailable, "the relay's journal of this agent is broken; the agent is refused until it is repaired")
	}
	ctx, release, err := a.attach(st.Context())
	if err != nil {
		return status.Error(codes.Unavailable, "the relay's journal of this agent is broken; the agent is refused until it is repaired")
	}
	defer release()
	if err := st.SendHeader(metadata.Pairs(reliable.EpochKey, a.out.Epoch())); err != nil {
		return err
	}
	a.log.Info("agent connected", "pending", a.out.Len())
	sess := reliable.Session{
		Out: a.out, In: a.in, PeerEpoch: epoch, AckEvery: h.ackEvery,
		Handle:    func(e *relayv1.Envelope) { h.inbound(a, e) },
		BeforeAck: a.syncJournal,
	}
	err = sess.Run(ctx, st)
	// Close waits for the receiving goroutine too, which may still be in
	// Handle after the handler returns.
	h.wg.Add(1)
	go func() {
		<-sess.Received()
		h.wg.Done()
	}()
	if errors.Is(context.Cause(ctx), errBroken) {
		return status.Error(codes.Unavailable, "the relay's journal of this agent is broken; the agent is refused until it is repaired")
	}
	if errors.Is(context.Cause(ctx), errReplaced) {
		a.log.Info("agent stream replaced by a newer one")
		return status.Error(codes.Aborted, errReplaced.Error())
	}
	if err != nil && st.Context().Err() == nil {
		a.log.Warn("agent stream failed", "err", err)
		return status.Error(codes.Unavailable, err.Error())
	}
	a.log.Info("agent disconnected")
	return nil
}

type leadService struct {
	relayv1.UnimplementedLeadServer
	h *Hub
}

// existing checks that name is an agent the allowlist has.
func (h *Hub) existing(name string) error {
	if !allowlist.ValidName(name) {
		return status.Errorf(codes.InvalidArgument, "not an agent name: %q", name)
	}
	ok, err := h.allowed(name)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "the allowlist: %v", err)
	}
	if !ok {
		return status.Errorf(codes.NotFound, "agent %q is not in the allowlist", name)
	}
	return nil
}

func checkText(text string) error {
	if text == "" {
		return status.Error(codes.InvalidArgument, "no text")
	}
	if len(text) > maxText {
		return status.Errorf(codes.InvalidArgument, "a text of %d bytes is over %d", len(text), maxText)
	}
	return nil
}

func (h *Hub) push(a *agentState, e *relayv1.Envelope) error {
	if err := a.out.Push(e); err != nil {
		a.log.Error("could not queue an envelope", "err", err)
		return status.Error(codes.Internal, "the relay could not queue it")
	}
	return nil
}

// Send queues a task; the agent runs it once connected.
func (s leadService) Send(_ context.Context, req *relayv1.SendRequest) (*relayv1.SendResponse, error) {
	h := s.h
	h.wg.Add(1)
	defer h.wg.Done()
	if err := h.existing(req.GetAgent()); err != nil {
		return nil, err
	}
	if err := checkText(req.GetText()); err != nil {
		return nil, err
	}
	a, err := h.agent(req.GetAgent())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "the relay's state of this agent is broken; the agent is quarantined: %s", text(err.Error(), maxError))
	}
	id := a.name + "/" + newID()
	// Open before it is queued, and durably: from the queue on, the agent
	// may answer for it.
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.refused(); err != nil {
		return nil, err
	}
	if len(a.st.tasks) >= maxOpenTasks {
		return nil, status.Errorf(codes.ResourceExhausted, "agent %s has %d open tasks; cancel some", a.name, len(a.st.tasks))
	}
	if err := a.record(record{Op: "task", held: held{ID: id}}); err != nil {
		return nil, status.Error(codes.Internal, "the relay could not record the task")
	}
	if err := a.st.sync(); err != nil {
		return nil, status.Error(codes.Internal, "the relay could not record the task")
	}
	if err := h.push(a, relayv1.Envelope_builder{Task: relayv1.Task_builder{Id: id, Text: req.GetText()}.Build()}.Build()); err != nil {
		_ = a.record(record{Op: "close", held: held{ID: id}})
		return nil, err
	}
	a.log.Info("task queued", "task", id)
	return relayv1.SendResponse_builder{TaskId: id}.Build(), nil
}

// Reply answers a pending question.
func (s leadService) Reply(_ context.Context, req *relayv1.ReplyRequest) (*relayv1.ReplyResponse, error) {
	h := s.h
	h.wg.Add(1)
	defer h.wg.Done()
	name, id, ok := splitID(req.GetQuestionId())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "not a question id (<agent>/<id>): %q", req.GetQuestionId())
	}
	if err := checkText(req.GetText()); err != nil {
		return nil, err
	}
	a, ok := h.known(name)
	if !ok {
		if err := h.isQuarantined(name); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "the relay's state of this agent is broken; the agent is quarantined: %s", text(err.Error(), maxError))
		}
		return nil, status.Errorf(codes.NotFound, "no question %q", req.GetQuestionId())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.refused(); err != nil {
		return nil, err
	}
	if _, ok := a.st.question(id); !ok {
		return nil, status.Errorf(codes.NotFound, "no pending question %q", req.GetQuestionId())
	}
	if err := h.push(a, relayv1.Envelope_builder{Reply: relayv1.Reply_builder{QuestionId: id, Text: req.GetText()}.Build()}.Build()); err != nil {
		return nil, err
	}
	_ = a.record(record{Op: "answered", held: held{ID: id}})
	return relayv1.ReplyResponse_builder{}.Build(), nil
}

// Cancel asks the agent to stop a task. The task leaves the open ones: the
// relay takes only the end of its turn (store.cancelled).
func (s leadService) Cancel(_ context.Context, req *relayv1.CancelRequest) (*relayv1.CancelResponse, error) {
	h := s.h
	h.wg.Add(1)
	defer h.wg.Done()
	name, _, ok := splitID(req.GetTaskId())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "not a task id (<agent>/<id>): %q", req.GetTaskId())
	}
	if err := h.existing(name); err != nil {
		return nil, err
	}
	a, err := h.agent(name)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "the relay's state of this agent is broken; the agent is quarantined: %s", text(err.Error(), maxError))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.refused(); err != nil {
		return nil, err
	}
	if !a.st.open(req.GetTaskId(), false) {
		return nil, status.Errorf(codes.NotFound, "no open task %q", req.GetTaskId())
	}
	if err := h.push(a, relayv1.Envelope_builder{Cancel: relayv1.Cancel_builder{TaskId: req.GetTaskId()}.Build()}.Build()); err != nil {
		return nil, err
	}
	_ = a.record(record{Op: "cancel", held: held{ID: req.GetTaskId()}})
	return relayv1.CancelResponse_builder{}.Build(), nil
}

// Watch streams what agents send: the pending questions and held messages
// first, then everything as it comes (watcher).
func (s leadService) Watch(req *relayv1.WatchRequest, st relayv1.Lead_WatchServer) error {
	h := s.h
	h.wg.Add(1)
	defer h.wg.Done()
	if req.GetAgent() != "" && !allowlist.ValidName(req.GetAgent()) {
		return status.Errorf(codes.InvalidArgument, "not an agent name: %q", req.GetAgent())
	}
	// Watching first, replaying second: whatever arrives in between is
	// either replayed or broadcast to this watcher (or both), never lost.
	w := h.watch(req.GetAgent())
	defer h.unwatch(w)
	h.replay(w)
	return w.run(st.Context(), st.Send, h.delivered)
}
