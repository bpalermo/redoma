package mcpconf

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bpalermo/redoma/xds/internal/egress"
)

// ToolCheck is what a tool an allowlist names, and the server does not
// list, does.
type ToolCheck string

const (
	// ToolCheckRefuse makes the file that names the tool invalid.
	ToolCheckRefuse ToolCheck = "refuse"
	// ToolCheckWarn logs it and publishes as if it were listed.
	ToolCheckWarn ToolCheck = "warn"
	// ToolCheckOff asks the server nothing.
	ToolCheckOff ToolCheck = "off"
)

// Header is a request header an upstream's route adds per agent. Name is
// lower case; "{tools}" in Value is the agent's tools, comma-separated.
type Header struct {
	Name, Value string
}

// Route sends tools/call of Tool to another upstream, Upstream.
type Route struct {
	Tool, Upstream string
}

// Upstream is one <etc>/mcp/<upstream>.conf.
type Upstream struct {
	Name  string
	Image string // by digest
	Port  uint32 // on 127.0.0.1 in the upstream's namespace
	Path  string // the server's MCP path
	Args  []string
	Env   []string // NAME=value
	// TokenFile is the credential's host path, under
	// /run/redoma-mcp/<upstream>/; empty when the upstream has none.
	TokenFile string
	Egress    []string // egress allowlist lines, port 443
	ToolCheck ToolCheck
	// StripHeaderPrefixes and StripHeaders are removed from every agent's
	// requests, lower case; StripHeaders always starts with authorization.
	StripHeaderPrefixes []string
	StripHeaders        []string
	AgentHeaders        []Header // in file order
	RouteFor            []Route  // in file order
	MaxRequestBytes     uint32
}

// The keys of an upstream's file. List keys may repeat; the dotted ones
// carry a name after the dot.
const (
	keyImage           = "image"
	keyPort            = "port"
	keyPath            = "path"
	keyArg             = "arg"
	keyEnv             = "env"
	keyTokenFile       = "token_file"
	keyEgress          = "egress"
	keyToolCheck       = "tool_check"
	keyStripPrefix     = "strip_header_prefix"
	keyStripHeader     = "strip_header"
	keyAgentHeader     = "agent_header."
	keyRouteFor        = "route_for."
	keyMaxRequestBytes = "max_request_bytes"
)

// TokenDir is where an upstream's credential lives on the host:
// /run/redoma-mcp/<upstream>/, outside /run/redoma, which the hook refuses
// to any container.
func TokenDir(upstream string) string {
	return "/run/redoma-mcp/" + upstream + "/"
}

// DefaultMaxRequestBytes and MaxRequestBytes bound the MCP filter's body
// limit (Envoy refuses more than 10 MiB).
const (
	DefaultMaxRequestBytes = 1 << 20
	MaxRequestBytes        = 10 << 20
)

var (
	// image is a reference by digest, sha256, and no tag: a ':' only for
	// the registry's port, in the first component.
	image = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)*@sha256:[0-9a-f]{64}$`)
	// envName is a variable's name.
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// secretName is a variable whose name says it holds a secret.
	secretName = regexp.MustCompile(`(?i)token|key|secret|password|credential`)
	// flag is an argument that is a flag, -x, --x or --x=value: its name.
	flag = regexp.MustCompile(`^--?([^=]+)`)
	// noVerify is a flag or variable whose name says it turns TLS
	// verification off.
	noVerify = regexp.MustCompile(`(?i)insecure|skip[-_]verify|^node_tls_reject_unauthorized$`)
	// headerName is an HTTP header field name (RFC 9110 token), no pseudo-
	// header.
	headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	// fileName is the credential file's own name.
	fileName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// keptHeaders are the request headers the protocol needs, which no strip
// line may remove. mcp-param- is a family.
var keptHeaders = []string{
	"accept", "connection", "content-length", "content-type", "host",
	"last-event-id", "mcp-method", "mcp-name", "mcp-param-",
	"mcp-protocol-version", "mcp-session-id", "te", "transfer-encoding",
}

// ReservedPorts are redoma's own listener ports, which an upstream's port
// may not be: the egress listener's in the upstream's own namespace (3128),
// and the agents' relay, model, MCP and AWS credentials (its default)
// listeners'.
var ReservedPorts = []uint32{3128, 7070, 8080, 8081, 9911}

// ReadUpstream reads upstream's file. A missing file is nil and no error.
func (d Dir) ReadUpstream(upstream string) (*Upstream, error) {
	if !ValidUpstream(upstream) {
		return nil, fmt.Errorf("%q is not an upstream name (lower-case letters, digits and -)", upstream)
	}
	file := d.UpstreamPath(upstream)
	lines, err := d.readLines(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u := &Upstream{
		Name:            upstream,
		Path:            "/",
		ToolCheck:       ToolCheckRefuse,
		StripHeaders:    []string{"authorization"},
		MaxRequestBytes: DefaultMaxRequestBytes,
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if err := u.set(l, seen); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", file, l.n, err)
		}
	}
	if u.Image == "" {
		return nil, fmt.Errorf("%s: no image", file)
	}
	if u.Port == 0 {
		return nil, fmt.Errorf("%s: no port", file)
	}
	return u, nil
}

// isList reports whether key may be given more than once.
func isList(key string) bool {
	switch key {
	case keyArg, keyEnv, keyEgress, keyStripPrefix, keyStripHeader:
		return true
	}
	return false
}

// set gives u one line's value. seen holds the keys given so far, a dotted
// key's name lower-cased for headers.
func (u *Upstream) set(l line, seen map[string]bool) error {
	key, value := l.key, l.value
	once := key
	if name, ok := strings.CutPrefix(key, keyAgentHeader); ok {
		once = keyAgentHeader + strings.ToLower(name)
	}
	if !isList(key) {
		if seen[once] {
			return fmt.Errorf("%s given twice", key)
		}
		seen[once] = true
	}
	switch {
	case isList(key):
		return u.add(key, value, seen)
	case strings.HasPrefix(key, keyAgentHeader):
		return u.setAgentHeader(strings.TrimPrefix(key, keyAgentHeader), value)
	case strings.HasPrefix(key, keyRouteFor):
		return u.setRoute(strings.TrimPrefix(key, keyRouteFor), value)
	}
	return u.setScalar(key, value)
}

// setScalar gives u the value of a key given once.
func (u *Upstream) setScalar(key, value string) error {
	switch key {
	case keyImage:
		if !image.MatchString(value) {
			return fmt.Errorf("image %q: give it by digest, <repository>@sha256:<64 hex>, never a tag", value)
		}
		u.Image = value
	case keyPort:
		return u.setPort(value)
	case keyPath:
		if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, " \t?#") {
			return fmt.Errorf("path %q is not an absolute URL path", value)
		}
		u.Path = value
	case keyTokenFile:
		return u.setTokenFile(value)
	case keyToolCheck:
		switch tc := ToolCheck(value); tc {
		case ToolCheckRefuse, ToolCheckWarn, ToolCheckOff:
			u.ToolCheck = tc
		default:
			return fmt.Errorf("tool_check %q is not refuse, warn or off", value)
		}
	case keyMaxRequestBytes:
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil || n == 0 || n > MaxRequestBytes {
			return fmt.Errorf("max_request_bytes %q is not 1-%d", value, MaxRequestBytes)
		}
		u.MaxRequestBytes = uint32(n)
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

// setPort takes the server's port: 1-65535, and none of redoma's own.
func (u *Upstream) setPort(value string) error {
	p, err := strconv.ParseUint(value, 10, 16)
	if err != nil || p == 0 {
		return fmt.Errorf("port %q is not 1-65535", value)
	}
	if slices.Contains(ReservedPorts, uint32(p)) {
		return fmt.Errorf("port %d is one of redoma's own listener ports %v", p, ReservedPorts)
	}
	u.Port = uint32(p)
	return nil
}

// add appends one line of a list key.
func (u *Upstream) add(key, value string, seen map[string]bool) error {
	switch key {
	case keyArg:
		if err := checkArg(value); err != nil {
			return err
		}
		u.Args = append(u.Args, value)
	case keyEnv:
		return u.setEnv(value, seen)
	case keyEgress:
		if err := egress.Check(value); err != nil {
			return fmt.Errorf("egress: %w", err)
		}
		u.Egress = append(u.Egress, value)
	case keyStripPrefix:
		p, err := strippable(value, true)
		if err != nil {
			return err
		}
		u.StripHeaderPrefixes = append(u.StripHeaderPrefixes, p)
	case keyStripHeader:
		h, err := strippable(value, false)
		if err != nil {
			return err
		}
		if !slices.Contains(u.StripHeaders, h) {
			u.StripHeaders = append(u.StripHeaders, h)
		}
	}
	return nil
}

// setEnv adds a NAME=value. A variable whose name says it holds a secret
// may name a file, never carry the value: the credential reaches the server
// only as a file.
func (u *Upstream) setEnv(value string, seen map[string]bool) error {
	name, _, ok := strings.Cut(value, "=")
	if !ok || !envName.MatchString(name) {
		// The name alone: a credential written with a typo in its name
		// must not reach the log.
		return fmt.Errorf("env %q is not NAME=value", name)
	}
	if noVerify.MatchString(name) {
		return fmt.Errorf("env %s turns the backend's TLS verification off", name)
	}
	if seen["env "+name] {
		return fmt.Errorf("env %s given twice", name)
	}
	seen["env "+name] = true
	upper := strings.ToUpper(name)
	if secretName.MatchString(name) && !strings.HasSuffix(upper, "_FILE") && !strings.HasSuffix(upper, "_PATH") {
		return fmt.Errorf("env %s looks like a secret: a credential reaches the server only as a file, so its name must end in _FILE or _PATH", name)
	}
	u.Env = append(u.Env, value)
	return nil
}

// checkArg refuses an empty argument, a flag that turns TLS verification
// off, and a flag whose name says it takes a secret unless it names a file
// (-file, -path, _file, _path): the credential reaches the server only as a
// file, and an argument is on the container's command line. The error
// names the flag, never its value.
func checkArg(value string) error {
	if value == "" {
		return errors.New("arg is empty")
	}
	m := flag.FindStringSubmatch(value)
	if m == nil {
		return nil
	}
	name := m[1]
	if noVerify.MatchString(name) {
		return fmt.Errorf("arg %s turns the backend's TLS verification off", m[0])
	}
	lower := strings.ToLower(name)
	for _, s := range []string{"-file", "-path", "_file", "_path"} {
		if strings.HasSuffix(lower, s) {
			return nil
		}
	}
	if secretName.MatchString(name) {
		return fmt.Errorf("arg %s looks like a secret: a credential reaches the server only as a file, so the flag must name one (-file, -path, _file or _path)", m[0])
	}
	return nil
}

// setTokenFile takes the credential's host path, a file directly in the
// upstream's own TokenDir.
func (u *Upstream) setTokenFile(value string) error {
	dir := TokenDir(u.Name)
	name, ok := strings.CutPrefix(value, dir)
	if !ok || !fileName.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("token_file %q is not a file directly in %s", value, dir)
	}
	u.TokenFile = value
	return nil
}

// strippable returns a strip line's header name or prefix, lower case,
// refusing one that would remove a header the protocol needs.
func strippable(value string, prefix bool) (string, error) {
	if !headerName.MatchString(value) {
		return "", fmt.Errorf("%q is not a header name", value)
	}
	h := strings.ToLower(value)
	for _, k := range keptHeaders {
		family := strings.HasSuffix(k, "-")
		if h == k || (prefix && strings.HasPrefix(k, h)) || (family && strings.HasPrefix(h, k)) {
			if family {
				k += "*"
			}
			return "", fmt.Errorf("stripping %q would remove %s, which the protocol needs", value, k)
		}
	}
	return h, nil
}

func (u *Upstream) setAgentHeader(name, value string) error {
	if !headerName.MatchString(name) {
		return fmt.Errorf("agent_header %q is not a header name", name)
	}
	if value == "" {
		return fmt.Errorf("agent_header.%s is empty", name)
	}
	h := strings.ToLower(name)
	if h == "authorization" || slices.Contains(keptHeaders, h) || strings.HasPrefix(h, "mcp-param-") {
		return fmt.Errorf("agent_header %s: redoma never sets %s for an agent", name, h)
	}
	u.AgentHeaders = append(u.AgentHeaders, Header{Name: h, Value: value})
	return nil
}

func (u *Upstream) setRoute(tool, target string) error {
	if !toolName.MatchString(tool) {
		return fmt.Errorf("route_for %q is not a tool name", tool)
	}
	if !ValidUpstream(target) {
		return fmt.Errorf("route_for.%s = %q is not an upstream name", tool, target)
	}
	if target == u.Name {
		return fmt.Errorf("route_for.%s routes the upstream to itself", tool)
	}
	u.RouteFor = append(u.RouteFor, Route{Tool: tool, Upstream: target})
	return nil
}
