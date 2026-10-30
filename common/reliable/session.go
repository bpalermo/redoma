package reliable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	relayv1 "github.com/bpalermo/redoma/api/redoma/relay/v1"
)

// EpochKey is the metadata key that carries a sender's queue epoch: the
// shim's in its request metadata, the relay's in its response header.
const EpochKey = "redoma-epoch"

// Stream is what a Session needs of a Connect stream, the client's or the
// server's.
type Stream interface {
	Send(*relayv1.Envelope) error
	Recv() (*relayv1.Envelope, error)
}

// Session runs one stream: it sends what Out has pending, then each push as
// it comes; it hands what the peer sends to Handle, once per seq (In); and it
// acks, at most every AckEvery, what Handle has taken.
type Session struct {
	Out *Outbox
	In  *Inbox
	// PeerEpoch is the epoch of the peer's queue, from its metadata.
	PeerEpoch string
	// Handle takes one new envelope from the peer, never an Ack or a
	// Heartbeat, in seq order. It runs on the goroutine that receives.
	Handle func(*relayv1.Envelope)
	// AckEvery is how often what was taken is saved and acked.
	AckEvery time.Duration
	// HeartbeatEvery is how often a Heartbeat is sent; 0 for none.
	HeartbeatEvery time.Duration
	// BeforeAck, if set, runs before what was taken is saved and acked:
	// whatever Handle wrote that the ack vouches for is made durable there.
	// An error skips this ack; the next tick tries again.
	BeforeAck func() error

	received chan struct{}
}

// Run runs the session until ctx ends, the stream fails or the peer ends it
// (nil then). One Session at a time may use an Outbox.
//
// The stream is the caller's to end once Run returns (a client cancels its
// context, a server handler returns): that is what ends the goroutine
// receiving, which may still be in Handle when Run returns.
func (s *Session) Run(ctx context.Context, st Stream) error {
	recvErr := make(chan error, 1)
	s.received = make(chan struct{})
	go func() {
		defer close(s.received)
		recvErr <- s.receive(st)
	}()
	err := s.send(ctx, st, recvErr)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// Received is closed once the goroutine receiving has ended (after Run has
// returned and the stream has been ended): Handle runs no more.
func (s *Session) Received() <-chan struct{} { return s.received }

func (s *Session) receive(st Stream) error {
	for {
		e, err := st.Recv()
		if err != nil {
			return err
		}
		switch e.WhichBody() {
		case relayv1.Envelope_Ack_case:
			if err := s.Out.Ack(e.GetAck().GetSeq()); err != nil {
				return fmt.Errorf("an ack: %w", err)
			}
		case relayv1.Envelope_Heartbeat_case:
		default:
			if e.GetSeq() == 0 {
				return errors.New("the peer sent an envelope without a seq")
			}
			s.In.Take(s.PeerEpoch, e.GetSeq(), func() { s.Handle(e) })
		}
	}
}

// sender is the sending half: one goroutine owns the stream's Send.
type sender struct {
	st   Stream
	out  *Outbox
	sent uint64
}

// flush sends every pending envelope not yet sent on this stream.
func (s *sender) flush() error {
	for _, e := range s.out.After(s.sent) {
		if err := s.st.Send(e); err != nil {
			return err
		}
		s.sent = e.GetSeq()
	}
	return nil
}

func (s *Session) send(ctx context.Context, st Stream, recvErr <-chan error) error {
	// What this side saved before is acked again, so the peer can drop it
	// rather than wait for its resends to be refused one by one.
	if last := s.In.Saved(s.PeerEpoch); last > 0 {
		if err := st.Send(ack(last)); err != nil {
			return err
		}
	}
	snd := &sender{st: st, out: s.Out}
	if err := snd.flush(); err != nil {
		return err
	}
	acks := time.NewTicker(s.AckEvery)
	defer acks.Stop()
	var beat <-chan time.Time
	if s.HeartbeatEvery > 0 {
		t := time.NewTicker(s.HeartbeatEvery)
		defer t.Stop()
		beat = t.C
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err = <-recvErr:
			return err
		case <-s.Out.Pushed():
			err = snd.flush()
		case <-acks.C:
			err = s.flushAck(st)
		case <-beat:
			err = st.Send(relayv1.Envelope_builder{Heartbeat: relayv1.Heartbeat_builder{}.Build()}.Build())
		}
		if err != nil {
			return err
		}
	}
}

func (s *Session) flushAck(st Stream) error {
	seq, ok, err := s.In.FlushAfter(s.BeforeAck)
	if err != nil || !ok {
		return err
	}
	return st.Send(ack(seq))
}

func ack(seq uint64) *relayv1.Envelope {
	return relayv1.Envelope_builder{Ack: relayv1.Ack_builder{Seq: seq}.Build()}.Build()
}
