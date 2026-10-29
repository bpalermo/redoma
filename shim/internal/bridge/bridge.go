// Package bridge links the shim's MCP server, a process claude starts for
// each turn (redoma-shim mcp), to the shim daemon that holds the stream to
// the relay: one request per connection, one JSON line each way, on a socket
// in the shim's state directory. Its address reaches the MCP server in the
// environment (EnvAddr).
package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// EnvAddr is the environment variable that carries the bridge's address:
// "unix:<path>", or "tcp:<host:port>" in tests.
const EnvAddr = "REDOMA_SHIM_BRIDGE"

// maxLine bounds a request or a response.
const maxLine = 2 << 20

// Lead is what the bridge serves: the running turn's line to the lead.
type Lead interface {
	// Ask sends a question and returns the lead's reply; it waits until
	// there is one, ctx ends, or the turn does.
	Ask(ctx context.Context, question string) (string, error)
	// Message sends the lead a message.
	Message(ctx context.Context, text string) error
}

type request struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

type response struct {
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// Addr is l's address as EnvAddr carries it.
func Addr(l net.Listener) string {
	return l.Addr().Network() + ":" + l.Addr().String()
}

// Serve answers requests on l until ctx ends.
func Serve(ctx context.Context, l net.Listener, lead Lead) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	var delay time.Duration
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Out of descriptors, a connection aborted before it was
			// accepted: wait and go on, as net/http does, rather than
			// leave the turns without ask_lead.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		go handle(ctx, conn, lead)
	}
}

func handle(ctx context.Context, conn net.Conn, lead Lead) {
	defer conn.Close()
	r := bufio.NewReader(io.LimitReader(conn, maxLine))
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		reply(conn, response{Error: "not a request"})
		return
	}
	// The caller going away (claude cancelled the tool call, or the turn
	// ended) ends the request: it sends nothing more, so a read returns
	// only at its close.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = r.ReadByte()
		cancel()
	}()
	var resp response
	switch req.Op {
	case "ask":
		resp.Text, err = lead.Ask(ctx, req.Text)
	case "message":
		err = lead.Message(ctx, req.Text)
	default:
		err = fmt.Errorf("no such op %q", req.Op)
	}
	if err != nil {
		resp = response{Error: err.Error()}
	}
	reply(conn, resp)
}

func reply(conn net.Conn, resp response) {
	b, _ := json.Marshal(resp)
	_, _ = conn.Write(append(b, '\n'))
}

// call sends one request to the bridge at addr and returns its answer.
func call(ctx context.Context, addr string, req request) (string, error) {
	network, address, ok := strings.Cut(addr, ":")
	if !ok || (network != "unix" && network != "tcp") {
		return "", fmt.Errorf("not a bridge address: %q", addr)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxLine)).ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("the shim: %w", err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return "", fmt.Errorf("the shim's answer: %w", err)
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.Text, nil
}

// Ask asks the lead through the bridge at addr.
func Ask(ctx context.Context, addr, question string) (string, error) {
	return call(ctx, addr, request{Op: "ask", Text: question})
}

// Message messages the lead through the bridge at addr.
func Message(ctx context.Context, addr, text string) error {
	_, err := call(ctx, addr, request{Op: "message", Text: text})
	return err
}
