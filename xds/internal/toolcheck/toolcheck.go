// Package toolcheck asks an MCP upstream for its tools
// (docs/mcp-upstreams.md, "The tool check"): initialize, then tools/list,
// following its cursor, through the host Envoy's check listener for the
// upstream. It sends no header but the protocol's, so the server lists its
// whole static ceiling.
package toolcheck

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
	"slices"
	"strings"
	"time"
)

// ProtocolVersion is the MCP protocol version the check speaks.
const ProtocolVersion = "2025-06-18"

// maxResponse bounds one response, and maxPages the pages of tools/list.
const (
	maxResponse = 4 << 20
	maxPages    = 100
)

// Dial connects to the check listener.
type Dial func(ctx context.Context) (net.Conn, error)

// Unix dials the unix socket at path.
func Unix(path string) Dial {
	return func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}
}

// Ask returns the names of the tools the upstream behind dial lists at
// path, sorted, each once. version names this client in initialize.
func Ask(ctx context.Context, dial Dial, path, version string) ([]string, error) {
	c := &client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
				DisableKeepAlives: true,
			},
			Timeout: 30 * time.Second,
		},
		url: "http://redoma-tool-check" + path,
	}
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "redoma-tool-check", "version": version},
	}); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return nil, fmt.Errorf("notifications/initialized: %w", err)
	}
	var names []string
	cursor := ""
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("tools/list: more than %d pages", maxPages)
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var res struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		for _, t := range res.Tools {
			names = append(names, t.Name)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// client is one session of the check.
type client struct {
	http    *http.Client
	url     string
	session string
	id      int
}

// call sends a request and returns its result.
func (c *client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.id++
	body, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("the response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil {
		return nil, errors.New("no result")
	}
	return resp.Result, nil
}

// notify sends a notification.
func (c *client) notify(ctx context.Context, method string) error {
	_, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "method": method})
	return err
}

// post sends one JSON-RPC message and returns the JSON of the answer: the
// body, or the last data line of an event stream. A notification's answer
// may be empty.
func (c *client) post(ctx context.Context, msg any) ([]byte, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponse {
		return nil, fmt.Errorf("a response over %d bytes", maxResponse)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 200)])))
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return lastData(data), nil
	}
	return data, nil
}

// lastData is the last data line of an event stream.
func lastData(stream []byte) []byte {
	var last []byte
	sc := bufio.NewScanner(bytes.NewReader(stream))
	sc.Buffer(nil, maxResponse)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
			last = []byte(strings.TrimSpace(d))
		}
	}
	return last
}
