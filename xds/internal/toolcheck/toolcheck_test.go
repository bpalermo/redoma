package toolcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeServer is an MCP server's Streamable HTTP endpoint at /mcp: it lists
// pages of tools, answers in JSON or in an event stream, and records what
// it was sent.
type fakeServer struct {
	mu      sync.Mutex
	pages   [][]string
	sse     bool
	session string
	fail    string // a method to answer with a JSON-RPC error
	status  int    // an HTTP status to answer everything with
	got     []string
	headers []http.Header
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     *int            `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(raw, &msg)
	f.mu.Lock()
	f.got = append(f.got, r.Method+" "+r.URL.Path+" "+msg.Method)
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	if msg.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg.Method {
	case "initialize":
		if f.session != "" {
			w.Header().Set("Mcp-Session-Id", f.session)
		}
		result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}}
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		page := 0
		if p.Cursor != "" {
			fmt.Sscanf(p.Cursor, "page-%d", &page)
		}
		var tools []map[string]any
		for _, n := range f.pages[page] {
			tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object"}})
		}
		res := map[string]any{"tools": tools}
		if page+1 < len(f.pages) {
			res["nextCursor"] = fmt.Sprintf("page-%d", page+1)
		}
		result = res
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result}
	if msg.Method == f.fail {
		// An error, and a result beside it: the error must win.
		resp["error"] = map[string]any{"code": -32601, "message": "no"}
	}
	body, _ := json.Marshal(resp)
	if f.status != 0 {
		// A refusal whose body still reads as an answer: the status must
		// win.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write(body)
		return
	}
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func serve(t *testing.T, f *fakeServer) Dial {
	t.Helper()
	s := httptest.NewServer(f)
	t.Cleanup(s.Close)
	addr := s.Listener.Addr().String()
	return func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", addr) }
}

// TestAsk: initialize, initialized, then tools/list page by page, the
// names sorted and each once, in JSON or an event stream; the session's id
// sent back once the server gives one; no header but the protocol's.
func TestAsk(t *testing.T) {
	for _, sse := range []bool{false, true} {
		f := &fakeServer{pages: [][]string{{"b", "a"}, {"c", "a"}, {}}, sse: sse, session: "s1"}
		got, err := Ask(t.Context(), serve(t, f), "/mcp/x", "0.0.0")
		if err != nil {
			t.Fatalf("sse %v: %v", sse, err)
		}
		if !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Errorf("sse %v: tools %q", sse, got)
		}
		want := []string{"POST /mcp/x initialize", "POST /mcp/x notifications/initialized", "POST /mcp/x tools/list", "POST /mcp/x tools/list", "POST /mcp/x tools/list"}
		if !slices.Equal(f.got, want) {
			t.Errorf("sse %v: requests %q, want %q", sse, f.got, want)
		}
		if f.headers[0].Get("Mcp-Session-Id") != "" || f.headers[1].Get("Mcp-Session-Id") != "s1" {
			t.Errorf("session ids %q, %q", f.headers[0].Get("Mcp-Session-Id"), f.headers[1].Get("Mcp-Session-Id"))
		}
		for _, h := range f.headers {
			for k := range h {
				switch strings.ToLower(k) {
				case "content-type", "accept", "mcp-protocol-version", "mcp-session-id", "content-length", "accept-encoding", "user-agent", "connection":
				default:
					t.Errorf("header %s sent", k)
				}
			}
		}
	}
}

func TestAskFails(t *testing.T) {
	for name, f := range map[string]*fakeServer{
		"a refused initialize": {pages: [][]string{{"a"}}, fail: "initialize"},
		"a refused tools/list": {pages: [][]string{{"a"}}, fail: "tools/list"},
		"a 403":                {pages: [][]string{{"a"}}, status: http.StatusForbidden},
		"a 404":                {pages: [][]string{{"a"}}, status: http.StatusNotFound},
	} {
		if got, err := Ask(t.Context(), serve(t, f), "/mcp/x", "0"); err == nil {
			t.Errorf("%s: %q, no error", name, got)
		}
	}
	// A cursor that never ends.
	f := &fakeServer{}
	for i := 0; i <= maxPages; i++ {
		f.pages = append(f.pages, []string{fmt.Sprint(i)})
	}
	f.pages = append(f.pages, []string{"last"})
	if _, err := Ask(t.Context(), serve(t, f), "/mcp/x", "0"); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Errorf("endless pages: %v", err)
	}
	// Nobody there.
	dial := func(ctx context.Context) (net.Conn, error) { return nil, fmt.Errorf("refused") }
	if _, err := Ask(t.Context(), dial, "/mcp/x", "0"); err == nil {
		t.Error("no server: no error")
	}
}
