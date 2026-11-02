// Package mcpe2e runs an agent's MCP listener in a real Envoy, in front of
// Grafana's MCP server (grafana/mcp-grafana), both the pinned release
// binaries of MODULE.bazel, with a fake Grafana behind the server. It is the
// proof, end to end, that docs/mcp-upstreams.md's Grafana instance holds:
// the server uses its configured token, whatever the agent sends; the
// headers that could override it never reach the server; a tool outside the
// agent's allowlist, or outside a constraint, is refused before the server;
// only the upstream's path, without a query, and only POST reach the
// server; a case-variant "Name" never runs the tool it names; and the
// server registers no write tool under --disable-write.
//
// Namespaces play no part here: the listener and the cluster are bound on
// the host's loopback (an empty namespace path), so the test runs
// unprivileged.
package mcpe2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/bpalermo/redoma/common/sockettest"
	"github.com/bpalermo/redoma/xds/internal/cds"
	"github.com/bpalermo/redoma/xds/internal/lds"
	"github.com/bpalermo/redoma/xds/internal/mcpconf"
	"github.com/bpalermo/redoma/xds/internal/toolcheck"
)

// grafana records what the fake Grafana was asked, and with which
// credential.
type grafana struct {
	mu       sync.Mutex
	requests []grafanaRequest
}

type grafanaRequest struct {
	path, authorization string
	grafanaHeaders      []string
}

func (g *grafana) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var xg []string
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-grafana-") {
			xg = append(xg, k)
		}
	}
	g.mu.Lock()
	g.requests = append(g.requests, grafanaRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization"), grafanaHeaders: xg})
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/search":
		fmt.Fprint(w, `[{"uid":"d1","title":"A dashboard","type":"dash-db"}]`)
	case strings.HasPrefix(r.URL.Path, "/api/datasources/uid/"):
		uid := strings.TrimPrefix(r.URL.Path, "/api/datasources/uid/")
		fmt.Fprintf(w, `{"id":1,"uid":%q,"name":"ds-%s","type":"prometheus","access":"proxy","url":"http://prom"}`, uid, uid)
	case r.URL.Path == "/api/datasources":
		fmt.Fprint(w, `[]`)
	default:
		fmt.Fprint(w, `{}`)
	}
}

// since returns the requests after the first n.
func (g *grafana) since(n int) []grafanaRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests[n:])
}

func (g *grafana) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

// reachedLog records every request that reaches the MCP server (the proxy
// in front of it, which Envoy's cluster points at): the path and query it
// asks for, and its headers.
type reachedLog struct {
	mu       sync.Mutex
	requests []reachedRequest
}

type reachedRequest struct {
	uri    string // path and query, as the server receives them
	header http.Header
}

func (h *reachedLog) add(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, reachedRequest{uri: r.URL.RequestURI(), header: r.Header.Clone()})
}

func (h *reachedLog) all() []reachedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.requests)
}

func (h *reachedLog) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// errExited is a process that exited before it listened, as one that lost
// its port to another process between freePort and its bind does.
var errExited = errors.New("exited before listening")

// waitPort waits until something accepts on port, or the process exits.
func waitPort(port int, exited <-chan error) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("%w: %v", errExited, err)
		default:
		}
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("not listening on %d", port)
}

// listen starts a process on a free port, run(port), and waits until it
// listens. freePort closes its probe before the process binds, so another
// process can take the port in between: the process then exits, and listen
// tries again on another port.
func listen(t *testing.T, what string, run func(port int) <-chan error) int {
	t.Helper()
	for attempt := 1; ; attempt++ {
		port := freePort(t)
		err := waitPort(port, run(port))
		if err == nil {
			return port
		}
		if !errors.Is(err, errExited) || attempt == 3 {
			t.Fatalf("%s: %v", what, err)
		}
		t.Logf("%s: %v; again on another port", what, err)
	}
}

// start runs a binary until the test ends, its output in the test's log
// when it fails.
func start(t *testing.T, name string, env []string, args ...string) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		<-exited
		if t.Failed() {
			t.Logf("%s output:\n%s", filepath.Base(name), out.String())
		}
	})
	return exited
}

// binary is the runfile env names (its rlocationpath).
func binary(t *testing.T, env string) string {
	t.Helper()
	p, root := os.Getenv(env), os.Getenv("TEST_SRCDIR")
	if p == "" || root == "" {
		t.Fatalf("%s or TEST_SRCDIR is not set (run under bazel test)", env)
	}
	return filepath.Join(root, p)
}

// The agent's allowlist: search anywhere, and one datasource by UID.
var allowlist = &mcpconf.Allowlist{Sets: []mcpconf.Set{
	{Tools: []mcpconf.Tool{{Name: "search_dashboards", Unconstrained: true}}},
	{Tools: []mcpconf.Tool{{Name: "get_datasource", Constraints: []mcpconf.Constraint{
		{Path: []string{"arguments", "uid"}, Value: "allowed-ds"},
	}}}},
}}

// writeTools are mcp-grafana v2.2.0's tools that create, change or delete,
// none of which --disable-write may leave registered.
var writeTools = []string{
	"update_dashboard", "create_folder", "create_datasource", "update_datasource",
	"create_incident", "update_incident", "add_activity_to_incident",
	"create_annotation", "update_annotation", "delete_annotation",
	"alerting_rules_write", "alerting_routing_write", "alerting_silences_write",
	"create_snapshot", "delete_snapshot", "update_alert_group", "install_plugin",
	"ask_assistant", "query_sql", "query_influxdb", "grafana_api_request",
}

type env struct {
	agentURL  string
	agentAddr string // 127.0.0.1:<port>, for raw requests
	grafana   *grafana
	reached   *reachedLog
	token     string
	// up is the upstream as the listeners have it, and dir the test's.
	up  *mcpconf.Upstream
	dir string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("configured-token-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := &grafana{}
	gs := httptest.NewServer(g)
	t.Cleanup(gs.Close)

	// mcp-grafana, as an upstream's unit runs it (docs/mcp-upstreams.md,
	// "Grafana": its ceiling and arguments, usage statistics off, so
	// nothing leaves loopback), on a loopback port of its own.
	serverPort := listen(t, "mcp-grafana", func(port int) <-chan error {
		return start(t, binary(t, "MCP_GRAFANA"), []string{
			"GRAFANA_URL=" + gs.URL,
			"GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=" + token,
			"HOME=" + dir,
		},
			"-t", "streamable-http", fmt.Sprintf("--address=127.0.0.1:%d", port), "--endpoint-path=/mcp",
			"--disable-write",
			"--enabled-tools=search,datasource,prometheus,loki,dashboard,alerting,navigation,tempo,pyroscope,annotations",
			"--disable-api", "--disable-admin", "--disable-incident", "--disable-oncall", "--disable-sql",
			"--disable-assistant", "--disable-rendering", "--disable-snapshot", "--disable-plugin",
			"--disable-provisioning",
			"--loki-guardrail-mode=enforce", "--prometheus-guardrail-mode=enforce",
			"--usage-stats=disabled",
		)
	})

	// A recording proxy in front of it: every path and header Envoy
	// forwards is what the server would read.
	reached := &reachedLog{}
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", serverPort))
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			reached.add(r.In)
			r.SetURL(target)
			r.Out.Host = target.Host
		},
		FlushInterval: -1,
	}
	ps := httptest.NewServer(proxy)
	t.Cleanup(ps.Close)
	proxyPort := ps.Listener.Addr().(*net.TCPAddr).Port

	up := &mcpconf.Upstream{
		Name: "grafana", Port: uint32(proxyPort), Path: "/mcp", MaxRequestBytes: mcpconf.DefaultMaxRequestBytes,
		StripHeaderPrefixes: []string{"x-grafana-"}, StripHeaders: []string{"authorization"},
	}
	agentPort := listen(t, "envoy", func(port int) <-chan error {
		listener := lds.MCPListener("", "a", []lds.MCPRoute{{Upstream: up, Allowlist: allowlist}}, nil, lds.StripOf([]*mcpconf.Upstream{up}))
		listener.GetAddress().GetSocketAddress().PortSpecifier = &corev3.SocketAddress_PortValue{PortValue: uint32(port)}
		bootstrap := &bootstrapv3.Bootstrap{
			Node: &corev3.Node{Id: "mcpe2e", Cluster: "mcpe2e"},
			StaticResources: &bootstrapv3.Bootstrap_StaticResources{
				Listeners: []*listenerv3.Listener{listener},
				Clusters:  []*clusterv3.Cluster{cds.MCPCluster("grafana", "", uint32(proxyPort))},
			},
		}
		raw, err := protojson.Marshal(bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(dir, "envoy.json")
		if err := os.WriteFile(config, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return start(t, binary(t, "ENVOY"), nil,
			"-c", config, "--disable-hot-restart", "--concurrency", "1", "-l", "warning", "--log-format", "%v")
	})

	return &env{
		agentURL:  fmt.Sprintf("http://127.0.0.1:%d%s", agentPort, lds.MCPPath("grafana")),
		agentAddr: fmt.Sprintf("127.0.0.1:%d", agentPort),
		grafana:   g,
		reached:   reached,
		token:     token,
		up:        up,
		dir:       dir,
	}
}

// TestToolCheck: redoma xds's tool check through the real Envoy's check
// listener, a unix socket: it lists the server's whole ceiling (the
// read tools, no write tool), and the listener refuses a tools/call. Skipped
// where unix sockets are refused (run in CI).
func TestToolCheck(t *testing.T) {
	probe, err := net.Listen("unix", filepath.Join(sockettest.Dir(t), "probe.sock"))
	sockettest.SkipIfRefused(t, err)
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	e := setup(t)
	socket := lds.CheckSocket(sockettest.Dir(t), "grafana")
	config := filepath.Join(e.dir, "check.json")
	raw, err := protojson.Marshal(&bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: "mcpe2e-check", Cluster: "mcpe2e"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Listeners: []*listenerv3.Listener{lds.CheckListener(e.up, socket)},
			Clusters:  []*clusterv3.Cluster{cds.MCPCluster("grafana", "", e.up.Port)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	exited := start(t, binary(t, "ENVOY"), nil,
		"-c", config, "--disable-hot-restart", "--concurrency", "1", "-l", "warning", "--log-format", "%v", "--base-id", "7")
	var tools []string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-exited:
			t.Fatalf("envoy exited: %v", err)
		default:
		}
		tools, err = toolcheck.Ask(t.Context(), toolcheck.Unix(socket), lds.MCPPath("grafana"), "test")
		if err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"search_dashboards", "list_datasources", "get_datasource"} {
		if !slices.Contains(tools, want) {
			t.Errorf("the check's tools %q lack %s", tools, want)
		}
	}
	for _, w := range writeTools {
		if slices.Contains(tools, w) {
			t.Errorf("the check lists the write tool %s", w)
		}
	}
	// A tools/call through the check listener is refused before the server.
	n := e.grafana.count()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return toolcheck.Unix(socket)(ctx)
	}}}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_dashboards","arguments":{}}}`
	req, err := http.NewRequest(http.MethodPost, "http://check"+lds.MCPPath("grafana"), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || e.grafana.count() != n {
		t.Errorf("a tools/call through the check listener: %d, Grafana asked %d times", resp.StatusCode, e.grafana.count()-n)
	}
}

// rpc is one JSON-RPC request through the agent's listener, with extra
// request headers. It returns the HTTP status and, for a 200, the
// JSON-RPC result or error.
func (e *env) rpc(t *testing.T, method string, params any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	return e.post(t, raw, headers)
}

func (e *env) post(t *testing.T, raw []byte, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.agentURL, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(nil, 1<<24)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				data = []byte(strings.TrimSpace(d))
			}
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return resp.StatusCode, nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: response %q: %v", raw, data, err)
	}
	return resp.StatusCode, out
}

// raw sends one HTTP/1.1 request to the agent's listener exactly as given,
// target and headers unparsed by a client, and returns the status.
func (e *env) raw(t *testing.T, method, target string, headers []string, body string) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", e.agentAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	req := method + " " + target + " HTTP/1.1\r\nHost: " + e.agentAddr + "\r\nConnection: close\r\n" +
		"Content-Type: application/json\r\nAccept: application/json, text/event-stream\r\n" +
		"MCP-Protocol-Version: 2025-06-18\r\n"
	for _, h := range headers {
		req += h + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"+body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// listDatasources is a tools/call of a registered tool the allowlist does
// not name: what a smuggled body would try.
const listDatasources = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_datasources","arguments":{}}}`

func (e *env) initialize(t *testing.T) {
	t.Helper()
	code, out := e.rpc(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcpe2e", "version": "0"},
	}, nil)
	if code != http.StatusOK || out["result"] == nil {
		t.Fatalf("initialize: %d %v", code, out)
	}
}

// callTool calls a tool and returns the HTTP status and whether the server
// reported the call an error.
func (e *env) callTool(t *testing.T, name string, args map[string]any, headers map[string]string) (int, bool) {
	t.Helper()
	code, out := e.rpc(t, "tools/call", map[string]any{"name": name, "arguments": args}, headers)
	if code != http.StatusOK {
		return code, false
	}
	res, _ := out["result"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	return code, out["error"] != nil || isErr
}

// The headers an agent might send to take over the server's credential or
// target: its own token, a token header, an API key, another Grafana, an
// org.
var override = map[string]string{
	"Authorization":                   "Bearer agent-token",
	"X-Grafana-Service-Account-Token": "agent-sa-token",
	"X-Grafana-API-Key":               "agent-api-key",
	"X-GRAFANA-URL":                   "http://203.0.113.1",
	"x-grafana-org-id":                "9",
}

func TestGrafanaUpstream(t *testing.T) {
	e := setup(t)
	e.initialize(t)

	t.Run("no write tool is registered", func(t *testing.T) {
		code, out := e.rpc(t, "tools/list", map[string]any{}, nil)
		if code != http.StatusOK {
			t.Fatalf("tools/list: %d", code)
		}
		res, _ := out["result"].(map[string]any)
		tools, _ := res["tools"].([]any)
		var names []string
		for _, tl := range tools {
			m, _ := tl.(map[string]any)
			n, _ := m["name"].(string)
			names = append(names, n)
		}
		if !slices.Contains(names, "search_dashboards") || !slices.Contains(names, "get_datasource") {
			t.Fatalf("tools/list %q lacks the allowlisted tools", names)
		}
		for _, w := range writeTools {
			if slices.Contains(names, w) {
				t.Errorf("write tool %s is registered under --disable-write", w)
			}
		}
	})

	t.Run("the configured token is used, whatever the agent sends", func(t *testing.T) {
		n := e.grafana.count()
		code, isErr := e.callTool(t, "search_dashboards", map[string]any{"query": "a"}, override)
		if code != http.StatusOK || isErr {
			t.Fatalf("search_dashboards: %d, error %v", code, isErr)
		}
		reqs := e.grafana.since(n)
		if len(reqs) == 0 {
			t.Fatal("Grafana was not asked")
		}
		for _, r := range reqs {
			if r.authorization != "Bearer configured-token-1" || len(r.grafanaHeaders) != 0 {
				t.Errorf("Grafana got %s with %q and %q", r.path, r.authorization, r.grafanaHeaders)
			}
		}
	})

	// Self-contained: it sends the override headers itself, so it proves
	// the stripping whichever subtests run before it.
	t.Run("authorization and x-grafana-* never reach the server", func(t *testing.T) {
		n := e.reached.count()
		if code, _ := e.rpc(t, "tools/list", map[string]any{}, override); code != http.StatusOK {
			t.Fatalf("tools/list: %d", code)
		}
		if code, isErr := e.callTool(t, "search_dashboards", map[string]any{"query": "c"}, override); code != http.StatusOK || isErr {
			t.Fatalf("search_dashboards: %d, error %v", code, isErr)
		}
		got := e.reached.all()[n:]
		if len(got) != 2 {
			t.Fatalf("%d requests reached the server, want 2", len(got))
		}
		for _, r := range got {
			for k := range r.header {
				lk := strings.ToLower(k)
				if lk == "authorization" || strings.HasPrefix(lk, "x-grafana-") {
					t.Errorf("the server got %s: %q", k, r.header.Values(k))
				}
			}
		}
	})

	// The route rewrites /mcp/grafana to the server's /mcp; nothing else
	// reaches it: no other path, no query, whatever the agent sends. Each of
	// these is refused (RBAC's 403, the catch-all's 404, or 400 for an
	// escaped slash) before the server.
	t.Run("only the upstream's path reaches the server", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_dashboards","arguments":{}}}`
		length := fmt.Sprintf("Content-Length: %d", len(body))
		for _, target := range []string{
			"/mcp/grafana/x",
			"/mcp/grafana/",
			"/mcp/grafana/..%2Fother",
			"/mcp/grafana%2Fx",
			"/mcp/grafana/../other",
			"/mcp/other",
			"/mcp/GRAFANA",
			"/mcp",
			"/",
			"/mcp/grafana?x=y",
			"/mcp/grafana?",
			"/mcp/grafana?Authorization=Bearer%20agent-token",
		} {
			n := e.reached.count()
			code := e.raw(t, "POST", target, []string{length}, body)
			if code != http.StatusForbidden && code != http.StatusNotFound && code != http.StatusBadRequest {
				t.Errorf("POST %s: %d, want it refused", target, code)
			}
			if e.reached.count() != n {
				t.Errorf("POST %s reached the server as %q", target, e.reached.all()[n].uri)
			}
		}
		// Merged slashes are the same path, so that one is allowed, and
		// reaches the server as its own /mcp.
		n := e.reached.count()
		if code := e.raw(t, "POST", "//mcp//grafana", []string{length}, body); code != http.StatusOK {
			t.Errorf("POST //mcp//grafana: %d, want 200", code)
		}
		if e.reached.count() != n+1 {
			t.Errorf("POST //mcp//grafana did not reach the server")
		}
		for _, r := range e.reached.all() {
			if r.uri != "/mcp" {
				t.Errorf("the server was asked for %q, want /mcp alone", r.uri)
			}
		}
	})

	// The MCP filter passes GET and DELETE before reading a body, and no
	// header tells RBAC one follows: Envoy's HTTP/1 codec removes
	// transfer-encoding before the filters run. So the route answers both
	// itself with a 405, body or not, and a tools/call smuggled in their
	// body never reaches the server. (A DELETE without Mcp-Session-Id is
	// the MCP filter's 400 before that.)
	t.Run("GET and DELETE never reach the server", func(t *testing.T) {
		chunked := fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(listDatasources), listDatasources)
		length := fmt.Sprintf("Content-Length: %d", len(listDatasources))
		for _, tc := range []struct {
			method  string
			headers []string
			body    string
			want    []int
		}{
			{"GET", nil, "", []int{http.StatusMethodNotAllowed}},
			{"DELETE", nil, "", []int{http.StatusMethodNotAllowed, http.StatusBadRequest}},
			{"DELETE", []string{"Mcp-Session-Id: s1"}, "", []int{http.StatusMethodNotAllowed}},
			{"GET", []string{length}, listDatasources, []int{http.StatusMethodNotAllowed}},
			{"GET", []string{"Transfer-Encoding: chunked"}, chunked, []int{http.StatusMethodNotAllowed}},
			{"DELETE", []string{length}, listDatasources, []int{http.StatusMethodNotAllowed, http.StatusBadRequest}},
			{"DELETE", []string{"Transfer-Encoding: chunked"}, chunked, []int{http.StatusMethodNotAllowed, http.StatusBadRequest}},
		} {
			n, g := e.reached.count(), e.grafana.count()
			if code := e.raw(t, tc.method, lds.MCPPath("grafana"), tc.headers, tc.body); !slices.Contains(tc.want, code) {
				t.Errorf("%s %q: %d, want one of %v", tc.method, tc.headers, code, tc.want)
			}
			if e.reached.count() != n || e.grafana.count() != g {
				t.Errorf("%s %q reached the server", tc.method, tc.headers)
			}
		}
	})

	t.Run("a rotated token is used without a restart", func(t *testing.T) {
		if err := os.WriteFile(e.token+".new", []byte("configured-token-2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(e.token+".new", e.token); err != nil {
			t.Fatal(err)
		}
		n := e.grafana.count()
		if code, isErr := e.callTool(t, "search_dashboards", map[string]any{"query": "b"}, nil); code != http.StatusOK || isErr {
			t.Fatalf("search_dashboards: %d, error %v", code, isErr)
		}
		for _, r := range e.grafana.since(n) {
			if r.authorization != "Bearer configured-token-2" {
				t.Errorf("Grafana got %s with %q", r.path, r.authorization)
			}
		}
	})

	t.Run("tools outside the allowlist are refused before the server", func(t *testing.T) {
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"list_datasources", map[string]any{}},    // registered, not allowed
			{"update_dashboard", map[string]any{}},    // a write tool
			{"grafana_api_request", map[string]any{}}, // the raw API
			{"get_datasource", map[string]any{"uid": "other-ds"}},
			{"get_datasource", map[string]any{"name": "allowed-ds"}}, // the constrained argument missing
		} {
			n, r := e.grafana.count(), e.reached.count()
			code, _ := e.callTool(t, tc.tool, tc.args, nil)
			if code != http.StatusForbidden {
				t.Errorf("%s %v: %d, want 403", tc.tool, tc.args, code)
			}
			if e.grafana.count() != n || e.reached.count() != r {
				t.Errorf("%s %v reached the server", tc.tool, tc.args)
			}
		}
	})

	t.Run("a constrained tool within its constraint", func(t *testing.T) {
		n := e.grafana.count()
		code, isErr := e.callTool(t, "get_datasource", map[string]any{"uid": "allowed-ds"}, nil)
		if code != http.StatusOK || isErr {
			t.Fatalf("get_datasource: %d, error %v", code, isErr)
		}
		reqs := e.grafana.since(n)
		if len(reqs) != 1 || reqs[0].path != "/api/datasources/uid/allowed-ds" {
			t.Errorf("Grafana got %+v", reqs)
		}
	})

	// A known limit, not a guarantee: Envoy reads "uid" with exact case,
	// and refuses only exact duplicate keys, but mcp-grafana v2.2.0 decodes
	// a tool's arguments with encoding/json (tools.go:177-191), which
	// matches keys case-insensitively, the last one winning. So "UID" next
	// to "uid" reaches past the constraint, and an argument constraint is
	// no boundary for this server (docs/mcp-upstreams.md, "A parser
	// differential"). The test holds that statement true: if a release
	// decodes with case, it fails, and the docs are to change with it.
	t.Run("a case-variant key reaches past a constraint (known)", func(t *testing.T) {
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_datasource","arguments":{"uid":"allowed-ds","UID":"other-ds"}}}`
		n := e.grafana.count()
		code, _ := e.post(t, []byte(raw), nil)
		reached := false
		for _, r := range e.grafana.since(n) {
			reached = reached || r.path == "/api/datasources/uid/other-ds"
		}
		if code != http.StatusOK || !reached {
			t.Errorf("%s: %d, other-ds reached %v: mcp-grafana no longer reads a case variant; update docs/mcp-upstreams.md and this test", raw, code, reached)
		}
	})

	// The rest of Grafana's boundary is the tool name, which RBAC reads with
	// exact case. It holds only because the server's dispatch (the MCP Go
	// SDK) decodes params case-exactly too: a "Name" next to "name" must
	// never run the tool it names. If a release changes the SDK or its
	// decoding, this fails.
	t.Run("a case-variant tool name never runs that tool", func(t *testing.T) {
		for _, raw := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_dashboards","Name":"list_datasources","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"Name":"list_datasources","name":"search_dashboards","arguments":{}}}`,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_dashboards","NAME":"list_datasources","arguments":{}}}`,
		} {
			n := e.grafana.count()
			code, _ := e.post(t, []byte(raw), nil)
			if code != http.StatusOK && code != http.StatusBadRequest {
				t.Errorf("%s: %d", raw, code)
			}
			searched := false
			for _, r := range e.grafana.since(n) {
				if r.path == "/api/datasources" {
					t.Errorf("%s ran list_datasources", raw)
				}
				searched = searched || r.path == "/api/search"
			}
			// A 200 proves the call reached the server: the tool it ran
			// is the one Envoy checked.
			if code == http.StatusOK && !searched {
				t.Errorf("%s: 200, but search_dashboards never asked Grafana", raw)
			}
		}
	})

	// The audit log's request_id is Envoy's, not the agent's: the agent is
	// an external client, so the x-request-id it sends is replaced.
	t.Run("the request id is Envoy's", func(t *testing.T) {
		n := e.reached.count()
		if code, _ := e.rpc(t, "tools/list", map[string]any{}, map[string]string{"X-Request-Id": "agent-chosen-id"}); code != http.StatusOK {
			t.Fatalf("tools/list: %d", code)
		}
		got := e.reached.all()[n:]
		if len(got) != 1 {
			t.Fatalf("%d requests reached the server, want 1", len(got))
		}
		if id := got[0].header.Get("X-Request-Id"); id == "" || id == "agent-chosen-id" {
			t.Errorf("the server got x-request-id %q, want one Envoy generated", id)
		}
	})

	t.Run("methods outside the lifecycle are refused", func(t *testing.T) {
		for _, m := range []string{"resources/list", "prompts/list", "completion/complete"} {
			if code, _ := e.rpc(t, m, map[string]any{}, nil); code != http.StatusForbidden {
				t.Errorf("%s: %d, want 403", m, code)
			}
		}
	})

	t.Run("a body that is not MCP is refused", func(t *testing.T) {
		if code, _ := e.post(t, []byte(`{"hello":"world"}`), nil); code != http.StatusBadRequest {
			t.Errorf("not JSON-RPC: %d, want 400", code)
		}
		dup := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_dashboards","name":"list_datasources","arguments":{}}}`)
		if code, _ := e.post(t, dup, nil); code != http.StatusBadRequest {
			t.Errorf("a duplicate key: %d, want 400", code)
		}
	})
}
