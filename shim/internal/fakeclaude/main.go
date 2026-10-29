// Command fakeclaude stands in for claude in the shim's tests. It takes the
// arguments the shim gives claude, reads the prompt on its standard input,
// prints stream-json, and acts on the prompt's first word:
//
//	echo <text>     an init line, an assistant line with the text, a result line
//	slow <n>        an init line, then n assistant lines 100ms apart, a result line
//	ask <question>  calls ask_lead through the MCP server of --mcp-config and
//	                prints the answer as an assistant line
//	message <text>  calls send_message the same way
//	sleep           an init line, then waits to be killed
//	fail            writes to its standard error and exits 3
//	badfail         the same, but what it writes is not UTF-8
//
//	spawn           an init line, then a child in a session of its own
//	                (setsid) that writes its pid to $FAKECLAUDE_PIDS and waits to
//	                be killed, then waits to be killed itself
//	leave           the same child, then a result line and exit 0
//
// The session is the --resume=<id> one, or NewSession; a resume of
// GoneSession fails at once with no output, as claude does for a session it
// does not have; a prompt with "crash" in it fails at once with no output,
// as an OOM kill would.
// Each run appends its arguments, a JSON array per line, to $FAKECLAUDE_LOG.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// flagValue is the value after name in the arguments, if any.
func flagValue(name string) string {
	for i := 1; i < len(os.Args)-1; i++ {
		if os.Args[i] == name {
			return os.Args[i+1]
		}
	}
	return ""
}

// The sessions fakeclaude knows.
const (
	NewSession  = "5c3f8a2e-4b1d-4e6f-9a7c-2d8e0f1b3a4c"
	GoneSession = "00000000-0000-4000-8000-000000000000"
)

func main() {
	if os.Getenv("FAKECLAUDE_CHILD") != "" {
		child()
		return
	}
	logArgs()
	mcpConfig, resume := flagValue("--mcp-config"), ""
	for _, a := range os.Args[1:] {
		if v, ok := strings.CutPrefix(a, "--resume="); ok {
			resume = v
		}
	}
	prompt, _ := io.ReadAll(os.Stdin)
	if strings.Contains(string(prompt), "crash") {
		fmt.Fprintln(os.Stderr, "Killed")
		os.Exit(137)
	}
	if resume == GoneSession {
		fmt.Fprintln(os.Stderr, "No conversation found with session ID: "+resume)
		os.Exit(1)
	}
	session := resume
	if session == "" {
		session = NewSession
	}
	word, rest, _ := strings.Cut(strings.TrimSpace(string(prompt)), " ")
	if word == "fail" {
		fmt.Fprintln(os.Stderr, "something broke")
		os.Exit(3)
	}
	if word == "badfail" {
		// Not UTF-8: a byte that starts no rune, and a rune cut short.
		_, _ = os.Stderr.Write([]byte("broke \xff\xfe and \xf0\x9f\x98"))
		os.Exit(3)
	}
	out(map[string]any{"type": "system", "subtype": "init", "session_id": session})
	act(word, rest, mcpConfig)
	out(map[string]any{"type": "result", "subtype": "success", "session_id": session})
}

func act(word, rest, mcpConfig string) {
	switch word {
	case "echo":
		say(rest)
	case "slow":
		n, _ := strconv.Atoi(rest)
		for i := range n {
			time.Sleep(100 * time.Millisecond)
			say(fmt.Sprintf("line %d", i))
		}
	case "ask", "message":
		tool, arg := "ask_lead", "question"
		if word == "message" {
			tool, arg = "send_message", "text"
		}
		text, err := callTool(mcpConfig, tool, map[string]string{arg: rest})
		if err != nil {
			fmt.Fprintln(os.Stderr, "the tool call:", err)
			os.Exit(4)
		}
		say(text)
	case "sleep":
		time.Sleep(time.Hour)
	case "spawn":
		spawn()
		time.Sleep(time.Hour)
	case "leave":
		spawn()
	}
}

// spawn starts a child in a session of its own and waits for it to write
// its pid.
func spawn() {
	self, _ := os.Executable()
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "FAKECLAUDE_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The child holds claude's output and error pipes, as a leftover
	// process would: the turn must not wait on them.
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		os.Exit(5)
	}
	for range 500 {
		if b, _ := os.ReadFile(os.Getenv("FAKECLAUDE_PIDS")); len(b) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// child writes its pid and waits to be killed; it ignores SIGTERM, so only
// the SIGKILL after the grace ends it.
func child() {
	signal.Ignore(syscall.SIGTERM)
	_ = os.WriteFile(os.Getenv("FAKECLAUDE_PIDS"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	time.Sleep(time.Hour)
}

func logArgs() {
	path := os.Getenv("FAKECLAUDE_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(os.Args[1:])
	_, _ = f.Write(append(b, '\n'))
}

func out(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func say(text string) {
	out(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}})
}

// callTool runs the MCP server of the config and calls one tool on it.
func callTool(config, tool string, args map[string]string) (string, error) {
	var c struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(config), &c); err != nil {
		return "", err
	}
	srv, ok := c.MCPServers["redoma"]
	if !ok {
		return "", fmt.Errorf("no redoma server in %s", config)
	}
	cmd := exec.Command(srv.Command, srv.Args...)
	cmd.Env = os.Environ()
	for k, v := range srv.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() {
		in.Close()
		_ = cmd.Wait()
	}()
	r := bufio.NewReader(stdout)
	send := func(m map[string]any) {
		m["jsonrpc"] = "2.0"
		b, _ := json.Marshal(m)
		_, _ = in.Write(append(b, '\n'))
	}
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}})
	if _, err := r.ReadBytes('\n'); err != nil {
		return "", err
	}
	send(map[string]any{"method": "notifications/initialized"})
	send(map[string]any{"id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	line, err := r.ReadBytes('\n')
	if err != nil {
		return "", err
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return "", err
	}
	if len(resp.Result.Content) == 0 {
		return "", fmt.Errorf("no content in %s", line)
	}
	if resp.Result.IsError {
		return "", fmt.Errorf("the tool failed: %s", resp.Result.Content[0].Text)
	}
	return resp.Result.Content[0].Text, nil
}
