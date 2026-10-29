package hub

import (
	"regexp"
	"time"
)

// What an agent may send. The agent side is untrusted: the turn runs as the
// shim's uid and can dial the relay listener itself, so everything below is
// enforced here, not assumed of the shim. An envelope over a limit or not
// valid is taken (acked, so it is not resent) and dropped, and the lead's
// watch gets a Dropped event.
const (
	// MaxRecvMsgSize is the agents' server's limit on one envelope.
	MaxRecvMsgSize = 256 << 10
	// maxChunk bounds a TurnEvent's chunk (the shim splits a longer line),
	// maxError its error.
	maxChunk = 192 << 10
	maxError = 4 << 10
	// maxQuestionText and maxMessageText bound a question's and a message's
	// text.
	maxQuestionText = 16 << 10
	maxMessageText  = 16 << 10
	// maxQuestions is how many questions an agent may have pending.
	maxQuestions = 32
	// maxHeldMessages and maxHeldBytes bound the messages held for the
	// lead, per agent, and maxHeldTotal the bytes across agents; a message's
	// bytes are its text and heldOverhead, what holding one costs besides.
	maxHeldMessages = 256
	heldOverhead    = 256
	maxHeldBytes    = 256 << 10
	maxHeldTotal    = 8 << 20
	// maxOpenTasks bounds the tasks the lead may have open with an agent.
	maxOpenTasks = 64

	// Per agent, turn output: bytes and events per second, with a burst.
	turnBytesPerSec  = 1 << 20
	turnBytesBurst   = 8 << 20
	turnEventsPerSec = 500
	turnEventsBurst  = 2000
	// Per agent, questions and messages together.
	notesPerSec = 5
	notesBurst  = 20

	// Per watch and agent: how far turn output may lag before it is
	// dropped (with a Dropped marker), and how many bytes of questions and
	// messages may wait.
	watchTurnBytes = 2 << 20
	watchNoteBytes = 1 << 20

	// markerEvery is how often a Dropped of an agent's inbound drops is
	// sent at most.
	markerEvery = time.Second
)

// questionID is what a shim's question id may be.
var questionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// bucket is a token bucket: rate tokens a second, at most burst.
type bucket struct {
	rate, burst float64
	tokens      float64
	last        time.Time
}

func newBucket(rate, burst float64) bucket {
	return bucket{rate: rate, burst: burst, tokens: burst}
}

// take takes n tokens, if there are.
func (b *bucket) take(n float64, now time.Time) bool {
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// heldSize is what a held message of text counts against the caps.
func heldSize(text string) int { return len(text) + heldOverhead }
