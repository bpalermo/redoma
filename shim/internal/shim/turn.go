package shim

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/common/atomicfile"
	"github.com/bpalermo/redoma/shim/internal/bridge"
)

// maxLine bounds one line of claude's output; maxChunk is the most of it one
// TurnEvent carries (the relay drops more): a longer line goes in parts.
const (
	maxLine  = 16 << 20
	maxChunk = 192 << 10
)

// mcpToolTimeout is MCP_TOOL_TIMEOUT for claude, in milliseconds: a day,
// since ask_lead waits for a human.
const mcpToolTimeout = "86400000"

// tools are the MCP tools claude may call without asking: the shim's.
const tools = "mcp__redoma__ask_lead,mcp__redoma__send_message"

// sessionUUID is what a claude session id is. The session file is in the
// workspace, which the turn can write: anything else in it is not resumed,
// so that it can never be read as a flag.
var sessionUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sessionNotFound is what claude says, on its standard error, of a --resume
// it has no session for.
const sessionNotFound = "No conversation found"

// runTask runs t's turn. When the resumed session does not exist (claude
// says so: a new container without the session's files), the session is
// forgotten and the turn runs once more in a new one; any other failure
// fails the turn and keeps the session.
func (s *Shim) runTask(t *turn) error {
	resume := s.sessionID()
	out, stderr, err := s.runClaude(t, resume)
	if err != nil && resume != "" && out == 0 && context.Cause(t.ctx) == nil && strings.Contains(stderr, sessionNotFound) {
		s.log.Warn("claude has no such session; starting a new one", "task", t.id, "session", resume)
		s.saveSession("")
		_, _, err = s.runClaude(t, "")
	}
	return err
}

func (s *Shim) mcpConfig() string {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"redoma": map[string]any{
		"type":    "stdio",
		"command": s.cfg.Self,
		"args":    []string{"mcp"},
		"env":     map[string]string{bridge.EnvAddr: bridge.Addr(s.cfg.Bridge)},
	}}})
	return string(b)
}

func (s *Shim) args(resume string) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--mcp-config", s.mcpConfig(), "--allowedTools", tools}
	if resume != "" {
		// One argument: a value can never be taken for a flag.
		args = append(args, "--resume="+resume)
	}
	return append(args, s.cfg.ClaudeArgs...)
}

// runClaude runs one claude process for t, the prompt on its standard
// input, and streams its output as TurnEvents. It returns how many lines
// there were and the end of claude's standard error.
//
// The turn is every process below the shim (reaper.Kill): a cancel ends
// them all, whatever their process group or session, and so does the end
// of claude, for whatever it left behind. Each signal goes to a process
// found below the shim just before, never to a remembered pid.
func (s *Shim) runClaude(t *turn, resume string) (int, string, error) {
	cmd := exec.Command(s.cfg.Claude, s.args(resume)...)
	cmd.Dir = s.cfg.Workdir
	cmd.Stdin = strings.NewReader(t.text)
	cmd.Env = append(os.Environ(), bridge.EnvAddr+"="+bridge.Addr(s.cfg.Bridge), "MCP_TOOL_TIMEOUT="+mcpToolTimeout)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Pipes of their own rather than StdoutPipe or a Writer as Stderr:
	// cmd.Wait then waits for claude alone, not for whatever else holds
	// them (a leftover process), and they are read to their end, which
	// comes once the turn's processes are killed (or closed here when
	// they cannot be).
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, "", err
	}
	defer pr.Close()
	er, ew, err := os.Pipe()
	if err != nil {
		pw.Close()
		return 0, "", err
	}
	defer er.Close()
	cmd.Stdout, cmd.Stderr = pw, ew
	err = s.cfg.Reaper.Start(cmd)
	pw.Close()
	ew.Close()
	if err != nil {
		return 0, "", fmt.Errorf("claude: %w", err)
	}
	stderr := &tail{max: 4096}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderr, er)
	}()
	var killErr error
	killed := make(chan struct{})
	stop := context.AfterFunc(t.ctx, func() {
		defer close(killed)
		killErr = s.cfg.Reaper.Kill(s.cfg.KillGrace)
	})
	linesc := make(chan int, 1)
	go func() { linesc <- s.stream(t, pr) }()
	err = cmd.Wait()
	s.cfg.Reaper.Done(cmd.Process.Pid)
	if stop() {
		// Not cancelled: what claude left running goes with its turn,
		// and with it the pipe's last writer.
		killErr = s.cfg.Reaper.Kill(s.cfg.KillGrace)
	} else {
		// Cancelled: the kill is done before the next turn starts, so it
		// can never reach that turn's processes.
		<-killed
	}
	if killErr != nil {
		// A survivor may hold the pipes: stop reading rather than hang.
		s.log.Error("the turn's processes survive; the next turn waits for them", "task", t.id, "err", killErr)
		pr.Close()
		er.Close()
	}
	lines := <-linesc
	<-stderrDone
	if cause := context.Cause(t.ctx); cause != nil {
		return lines, stderr.String(), cause
	}
	if killErr != nil {
		return lines, stderr.String(), killErr
	}
	if err != nil {
		return lines, stderr.String(), fmt.Errorf("claude: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return lines, stderr.String(), nil
}

// stream sends each line of r as TurnEvents, a line over maxChunk in parts,
// and keeps the session id the lines carry.
func (s *Shim) stream(t *turn, r io.Reader) int {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		n++
		for len(line) > maxChunk {
			s.push(turnEvent(t.id, line[:maxChunk], true))
			line = line[maxChunk:]
		}
		s.push(turnEvent(t.id, line, false))
		if id := sessionOf(sc.Bytes()); id != "" && id != s.sessionID() {
			s.saveSession(id)
		}
	}
	if err := sc.Err(); err != nil {
		s.log.Error("claude's output could not be read to its end", "task", t.id, "err", err)
		// Drain, so claude is not stuck on a full pipe.
		_, _ = io.Copy(io.Discard, r)
	}
	return n
}

func turnEvent(task string, chunk []byte, partial bool) *relayv1.Envelope {
	return &relayv1.Envelope{Body: &relayv1.Envelope_TurnEvent{TurnEvent: &relayv1.TurnEvent{
		TaskId: task, Chunk: bytes.Clone(chunk), Partial: partial,
	}}}
}

// sessionOf is the session id of a stream-json line that carries one (its
// init and its result), if it is one.
func sessionOf(line []byte) string {
	if !bytes.Contains(line, []byte(`"session_id"`)) {
		return ""
	}
	var m struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(line, &m) != nil || !sessionUUID.MatchString(m.SessionID) {
		return ""
	}
	return m.SessionID
}

// sessionID is the saved session, if it is one; anything else is logged and
// forgotten.
func (s *Shim) sessionID() string {
	b, err := os.ReadFile(s.path("session"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Error("the session file is unreadable; starting a new session", "err", err)
		}
		return ""
	}
	id := strings.TrimSpace(string(b))
	if !sessionUUID.MatchString(id) {
		s.log.Error("the session file holds no session id; starting a new session", "len", len(b))
		s.saveSession("")
		return ""
	}
	return id
}

func (s *Shim) saveSession(id string) {
	var err error
	if id == "" {
		err = os.Remove(s.path("session"))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = atomicfile.Write(s.path("session"), []byte(id+"\n"), 0o600)
	}
	if err != nil {
		s.log.Error("could not save the session id", "err", err)
	}
}

// tail keeps the last max bytes written to it.
type tail struct {
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.buf) }
