package mcpconf

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// testDir is an etc directory a test owns, with mcp/ in it.
func testDir(t *testing.T) Dir {
	t.Helper()
	etc := t.TempDir()
	if err := os.Mkdir(filepath.Join(etc, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Dir{Etc: etc, Owner: uint32(os.Getuid())}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const grafana = `# a comment
image = docker.io/grafana/mcp-grafana@` + digest + `
port = 8000
path = /mcp
tool_check = warn
arg = -t
arg = streamable-http
arg = --address=127.0.0.1:8000
env = GRAFANA_URL=https://grafana.example.com
env = GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=/run/secrets/grafana/token
token_file = /run/redoma-mcp/grafana/token
egress = grafana.example.com
egress = *.grafana.example.org
strip_header_prefix = X-Grafana-
strip_header = x-forwarded-user
agent_header.X-Agent-Tools = {tools}
route_for.create_thing = grafana-writer
max_request_bytes = 65536
`

func TestReadUpstream(t *testing.T) {
	d := testDir(t)
	write(t, d.UpstreamPath("grafana"), grafana)
	got, err := d.ReadUpstream("grafana")
	if err != nil {
		t.Fatal(err)
	}
	want := &Upstream{
		Name:                "grafana",
		Image:               "docker.io/grafana/mcp-grafana@" + digest,
		Port:                8000,
		Path:                "/mcp",
		Args:                []string{"-t", "streamable-http", "--address=127.0.0.1:8000"},
		Env:                 []string{"GRAFANA_URL=https://grafana.example.com", "GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=/run/secrets/grafana/token"},
		TokenFile:           "/run/redoma-mcp/grafana/token",
		Egress:              []string{"grafana.example.com", "*.grafana.example.org"},
		ToolCheck:           ToolCheckWarn,
		StripHeaderPrefixes: []string{"x-grafana-"},
		StripHeaders:        []string{"authorization", "x-forwarded-user"},
		AgentHeaders:        []Header{{Name: "x-agent-tools", Value: "{tools}"}},
		RouteFor:            []Route{{Tool: "create_thing", Upstream: "grafana-writer"}},
		MaxRequestBytes:     65536,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadUpstream =\n%+v\nwant\n%+v", got, want)
	}
}

func TestReadUpstreamDefaults(t *testing.T) {
	d := testDir(t)
	write(t, d.UpstreamPath("u"), "image = example.org/u@"+digest+"\nport = 8082\n")
	got, err := d.ReadUpstream("u")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/" || got.ToolCheck != ToolCheckRefuse || got.MaxRequestBytes != 1<<20 ||
		!slices.Equal(got.StripHeaders, []string{"authorization"}) || got.TokenFile != "" {
		t.Errorf("defaults: %+v", got)
	}
}

func TestReadUpstreamMissingIsNil(t *testing.T) {
	d := testDir(t)
	if got, err := d.ReadUpstream("nobody"); got != nil || err != nil {
		t.Errorf("ReadUpstream = %v, %v; want nil, nil", got, err)
	}
}

// Every refusal names what is wrong. A file is refused whole: an upstream
// with a bad line is not half configured.
func TestReadUpstreamRefuses(t *testing.T) {
	base := "image = example.org/u@" + digest + "\nport = 8082\n"
	for _, tc := range []struct{ name, content, want string }{
		{"no image", "port = 8082\n", "no image"},
		{"no port", "image = example.org/u@" + digest + "\n", "no port"},
		{"a tag", "image = example.org/u:v1\nport = 1\n", "by digest"},
		{"a tag and a digest", "image = example.org/u:v1@" + digest + "\nport = 1\n", "by digest"},
		{"a tag and a digest, with a registry port", "image = example.org:5000/u:v1@" + digest + "\nport = 1\n", "by digest"},
		{"a port past the first component", "image = example.org/a:1/b@" + digest + "\nport = 1\n", "by digest"},
		{"a short digest", "image = example.org/u@sha256:abc\nport = 1\n", "by digest"},
		{"a long digest", "image = example.org/u@" + digest + "0\nport = 1\n", "by digest"},
		{"an upper-case digest", "image = example.org/u@" + strings.ToUpper(digest) + "\nport = 1\n", "by digest"},
		{"port of the egress listener", "image = example.org/u@" + digest + "\nport = 3128\n", "redoma's own listener ports"},
		{"port of the relay listener", "image = example.org/u@" + digest + "\nport = 7070\n", "redoma's own listener ports"},
		{"port of the model listener", "image = example.org/u@" + digest + "\nport = 8080\n", "redoma's own listener ports"},
		{"port of the MCP listener", "image = example.org/u@" + digest + "\nport = 8081\n", "redoma's own listener ports"},
		{"port of the AWS listener", "image = example.org/u@" + digest + "\nport = 9911\n", "redoma's own listener ports"},
		{"arg token value", base + "arg = --token=s3cr3t\n", "looks like a secret"},
		{"arg token, value apart", base + "arg = --token\narg = s3cr3t\n", "looks like a secret"},
		{"arg api key", base + "arg = --api-key=s3cr3t\n", "looks like a secret"},
		{"arg single dash", base + "arg = -password=s3cr3t\n", "looks like a secret"},
		{"arg any case", base + "arg = --Server-Auth-Token=s3cr3t\n", "looks like a secret"},
		{"arg credentials", base + "arg = --credentials=s3cr3t\n", "looks like a secret"},
		{"arg skip verify", base + "arg = --tls-skip-verify\n", "TLS verification off"},
		{"arg skip verify, underscore", base + "arg = --Skip_Verify=true\n", "TLS verification off"},
		{"arg insecure", base + "arg = --insecure\n", "TLS verification off"},
		{"arg insecure file", base + "arg = --insecure-file=/x\n", "TLS verification off"},
		{"env insecure", base + "env = GIT_SSL_INSECURE=1\n", "TLS verification off"},
		{"env skip verify", base + "env = TLS_SKIP_VERIFY=1\n", "TLS verification off"},
		{"env node reject unauthorized", base + "env = NODE_TLS_REJECT_UNAUTHORIZED=0\n", "TLS verification off"},
		{"arg trailing comment", base + "arg = --flag # note\n", "starts a comment only at the start of a line"},
		{"env trailing comment", base + "env = A=b\t# note\n", "starts a comment only at the start of a line"},
		{"agent header trailing comment", base + "agent_header.X-A = v # note\n", "starts a comment only at the start of a line"},
		{"port zero", "image = example.org/u@" + digest + "\nport = 0\n", "1-65535"},
		{"port too big", "image = example.org/u@" + digest + "\nport = 65536\n", "port"},
		{"unknown key", base + "colour = blue\n", `unknown key "colour"`},
		{"twice", base + "port = 1\n", "port given twice"},
		{"no equals", base + "arg\n", "not `key = value`"},
		{"relative path", base + "path = mcp\n", "path"},
		{"path with space", base + "path = /a b\n", "path"},
		{"empty arg", base + "arg =\n", "empty"},
		{"env without =", base + "env = NAME\n", "NAME=value"},
		{"env bad name", base + "env = 1X=y\n", "NAME=value"},
		{"env twice", base + "env = A=1\nenv = A=2\n", "A given twice"},
		{"env token value", base + "env = GRAFANA_SERVICE_ACCOUNT_TOKEN=abc\n", "_FILE or _PATH"},
		{"env api key", base + "env = API_KEY=abc\n", "_FILE or _PATH"},
		{"env secret", base + "env = MY_SECRET=abc\n", "_FILE or _PATH"},
		{"env password", base + "env = DB_PASSWORD=abc\n", "_FILE or _PATH"},
		{"env credential", base + "env = Credentials=abc\n", "_FILE or _PATH"},
		{"token file elsewhere", base + "token_file = /etc/shadow\n", "/run/redoma-mcp/u/"},
		{"token file other upstream", base + "token_file = /run/redoma-mcp/v/token\n", "/run/redoma-mcp/u/"},
		{"token file traversal", base + "token_file = /run/redoma-mcp/u/../v/token\n", "/run/redoma-mcp/u/"},
		{"token file deeper", base + "token_file = /run/redoma-mcp/u/a/token\n", "/run/redoma-mcp/u/"},
		{"bad egress", base + "egress = *.com\n", "wildcard"},
		{"egress with port", base + "egress = example.org:443\n", "not a host name"},
		{"tool_check", base + "tool_check = maybe\n", "tool_check"},
		{"empty prefix", base + "strip_header_prefix =\n", "header"},
		{"prefix of a kept header", base + "strip_header_prefix = mcp-\n", "the protocol needs"},
		{"prefix of host", base + "strip_header_prefix = ho\n", "host"},
		{"strip a kept header", base + "strip_header = Content-Type\n", "content-type"},
		{"strip a kept family", base + "strip_header = Mcp-Param-Owner\n", "mcp-param-*"},
		{"strip a pseudo-header", base + "strip_header = :path\n", "header"},
		{"agent header bad name", base + "agent_header.X Y = 1\n", "header"},
		{"agent header empty", base + "agent_header.X-Y =\n", "empty"},
		{"agent header authorization", base + "agent_header.Authorization = x\n", "authorization"},
		{"agent header kept", base + "agent_header.Mcp-Session-Id = x\n", "mcp-session-id"},
		{"agent header twice", base + "agent_header.X-A = 1\nagent_header.x-a = 2\n", "given twice"},
		{"route bad tool", base + "route_for.a b = v\n", "tool name"},
		{"route bad upstream", base + "route_for.t = V_W\n", "upstream name"},
		{"route to itself", base + "route_for.t = u\n", "itself"},
		{"route twice", base + "route_for.t = v\nroute_for.t = w\n", "given twice"},
		{"max zero", base + "max_request_bytes = 0\n", "max_request_bytes"},
		{"max too big", base + "max_request_bytes = 10485761\n", "max_request_bytes"},
		{"carriage return", strings.ReplaceAll(base, "\n", "\r\n"), "carriage return"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDir(t)
			write(t, d.UpstreamPath("u"), tc.content)
			got, err := d.ReadUpstream("u")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ReadUpstream = %+v, %v; want an error with %q", got, err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), d.UpstreamPath("u")) {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

// What the arg and env checks let through: a flag that names a file, a
// value that is not a flag, a # with no space before it.
func TestReadUpstreamAllows(t *testing.T) {
	base := "image = example.org:5000/a/b_c.d@" + digest + "\nport = 8082\n"
	for _, l := range []string{
		"arg = --token-file=/run/secrets/u/token",
		"arg = --tls-key-file=/run/secrets/u/key",
		"arg = --Key_Path=/run/secrets/u/key",
		"arg = --api-key-path",
		"arg = -t",
		"arg = streamable-http",
		"arg = token",
		"arg = --enabled-tools=search,datasource",
		"arg = --color=#fff",
		"env = API_TOKEN_FILE=/run/secrets/u/token",
		"env = A=b#c",
	} {
		d := testDir(t)
		write(t, d.UpstreamPath("u"), base+l+"\n")
		if _, err := d.ReadUpstream("u"); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
}

// An error never quotes a value that may be a credential: a refused arg
// names its flag, a refused env its name.
func TestReadUpstreamErrorsHoldNoValue(t *testing.T) {
	base := "image = example.org/u@" + digest + "\nport = 8082\n"
	for _, l := range []string{
		"env = API-KEY=s3cr3t",
		"env = 1X=s3cr3t",
		"env = API_KEY=s3cr3t",
		"arg = --token=s3cr3t",
		"arg = --tls-skip-verify=s3cr3t",
		"env = GIT_SSL_INSECURE=s3cr3t",
	} {
		d := testDir(t)
		write(t, d.UpstreamPath("u"), base+l+"\n")
		_, err := d.ReadUpstream("u")
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: err = %v; want an error without the value", l, err)
		}
	}
}

// A FIFO where a file should be fails at once, from the check before the
// open; it never blocks the scan, even in a directory someone else could
// write.
func TestReadUpstreamRefusesAFIFO(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o777} {
		d := testDir(t)
		if err := syscall.Mkfifo(d.UpstreamPath("u"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(d.Etc, "mcp"), mode); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := d.ReadUpstream("u")
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("directory %v: a FIFO: err = nil", mode)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("directory %v: ReadUpstream blocked on a FIFO", mode)
		}
	}
}

func TestReadUpstreamRefusesBadNames(t *testing.T) {
	d := testDir(t)
	for _, name := range []string{"", "_default", "A", "a/b", "..", strings.Repeat("a", 42)} {
		if got, err := d.ReadUpstream(name); err == nil {
			t.Errorf("ReadUpstream(%q) = %v, nil; want an error", name, got)
		}
	}
}

// The files are root's and writable by no one else, like agents.txt: one
// another user could have changed configures nothing.
func TestReadUpstreamRefusesAnUntrustedFile(t *testing.T) {
	d := testDir(t)
	write(t, d.UpstreamPath("u"), "image = example.org/u@"+digest+"\nport = 8082\n")
	if err := os.Chmod(d.UpstreamPath("u"), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadUpstream("u"); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Errorf("a group-writable file: err = %v", err)
	}

	d = testDir(t)
	write(t, d.UpstreamPath("u"), "image = example.org/u@"+digest+"\nport = 8082\n")
	if err := os.Chmod(filepath.Join(d.Etc, "mcp"), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadUpstream("u"); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Errorf("a world-writable directory: err = %v", err)
	}

	d = testDir(t)
	write(t, d.UpstreamPath("u"), "image = example.org/u@"+digest+"\nport = 8082\n")
	d.Owner++
	if _, err := d.ReadUpstream("u"); err == nil || !strings.Contains(err.Error(), "owned") {
		t.Errorf("another owner: err = %v", err)
	}

	d = testDir(t)
	target := filepath.Join(d.Etc, "elsewhere.conf")
	write(t, target, "image = example.org/u@"+digest+"\nport = 8082\n")
	if err := os.Symlink(target, d.UpstreamPath("u")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadUpstream("u"); err == nil {
		t.Error("a symlink: err = nil")
	}
}

func TestUpstreams(t *testing.T) {
	d := testDir(t)
	write(t, d.UpstreamPath("b"), "")
	write(t, d.UpstreamPath("a"), "")
	write(t, filepath.Join(d.Etc, "mcp", "notes.txt"), "")
	write(t, filepath.Join(d.Etc, "mcp", "a", "x.conf"), "")
	write(t, filepath.Join(d.Etc, "mcp", "Bad_Name.conf"), "")
	got, err := d.Upstreams()
	if err != nil {
		t.Fatal(err)
	}
	// Every .conf, valid name or not: a bad name is ReadUpstream's error
	// to report, not a file to skip silently.
	if want := []string{"Bad_Name", "a", "b"}; !slices.Equal(got, want) {
		t.Errorf("Upstreams = %q, want %q", got, want)
	}
}

func TestUpstreamsWithoutTheDirectory(t *testing.T) {
	d := Dir{Etc: t.TempDir(), Owner: uint32(os.Getuid())}
	if got, err := d.Upstreams(); got != nil || err != nil {
		t.Errorf("Upstreams = %q, %v; want nil, nil", got, err)
	}
}
