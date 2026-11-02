package mcpconf

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bpalermo/redoma/common/allowlist"
)

// Constraint is one argument a tool may be called with: the argument at
// Path (under params, starting with "arguments") is Value, or starts with
// it when Prefix. The argument must be present.
type Constraint struct {
	Path   []string
	Value  string
	Prefix bool
}

// Tool is a tool an allowlist's set allows, under all its constraints.
// Unconstrained is a tool_unconstrained line, allowed with any arguments;
// it is the only way to say so. A Tool with no constraints that is not
// Unconstrained allows nothing: empty constraints never mean any
// arguments.
type Tool struct {
	Name          string
	Constraints   []Constraint
	Unconstrained bool
}

// Set is one set of an allowlist: its tools, each under its constraints.
type Set struct {
	Tools []Tool
}

// Allowlist is an agent's tools for one upstream, the defaults' sets then
// the agent's own. A call is allowed when any set allows it.
type Allowlist struct {
	Sets []Set
}

// Tools returns every tool name the allowlist names, sorted, each once.
func (a *Allowlist) Tools() []string {
	var names []string
	for _, s := range a.Sets {
		for _, t := range s.Tools {
			names = append(names, t.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Arguments returns every argument path a constraint names, dotted
// ("arguments.owner"), sorted, each once: what the MCP filter must extract.
func (a *Allowlist) Arguments() []string {
	var paths []string
	for _, s := range a.Sets {
		for _, t := range s.Tools {
			for _, c := range t.Constraints {
				paths = append(paths, strings.Join(c.Path, "."))
			}
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// The keys of an allowlist.
const (
	keyTool              = "tool"
	keyToolUnconstrained = "tool_unconstrained"
	keyConstrain         = "constrain"
	sectionSet           = "[set]"
)

// argumentPath is a constraint's path: arguments, then one or more keys.
var argumentPath = regexp.MustCompile(`^arguments(\.[A-Za-z0-9_-]+)+$`)

// ReadAllowlist returns agent's allowlist for upstream: the defaults' sets,
// then the agent's own. Without the agent's own file it is nil and no
// error: the agent has no route to the upstream, whatever the defaults
// say. An invalid file, the defaults' or the agent's, is an error.
func (d Dir) ReadAllowlist(upstream, agent string) (*Allowlist, error) {
	if !ValidUpstream(upstream) {
		return nil, fmt.Errorf("%q is not an upstream name", upstream)
	}
	if !allowlist.ValidName(agent) {
		return nil, fmt.Errorf("%q is not an agent name", agent)
	}
	own, err := d.readSets(d.AllowlistPath(upstream, agent))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defaults, err := d.readSets(d.AllowlistPath(upstream, Default))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return &Allowlist{Sets: slices.Concat(defaults, own)}, nil
}

// ReadDefaults returns upstream's defaults' allowlist, _default.conf alone:
// nil and no error without the file.
func (d Dir) ReadDefaults(upstream string) (*Allowlist, error) {
	if !ValidUpstream(upstream) {
		return nil, fmt.Errorf("%q is not an upstream name", upstream)
	}
	sets, err := d.readSets(d.AllowlistPath(upstream, Default))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Allowlist{Sets: sets}, nil
}

// Agents returns the name of every agent with an allowlist file for
// upstream, <etc>/mcp/<upstream>/<agent>.conf, sorted; not the defaults',
// nor a name that is not an agent's. Without the directory, none.
func (d Dir) Agents(upstream string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(d.Etc, "mcp", upstream))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".conf"); ok && !e.IsDir() && allowlist.ValidName(name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// setBuilder is a set being read: its tools in file order, the line of
// each tool = line (the tools that take constraints), and the line of its
// [set] (0 for the lines before the first).
type setBuilder struct {
	set         Set
	constrained map[string]int
	n           int
}

// readSets reads one allowlist file into its sets.
func (d Dir) readSets(file string) ([]Set, error) {
	lines, err := d.readLines(file)
	if err != nil {
		return nil, err
	}
	r := &setsReader{cur: newSetBuilder(0)}
	for _, l := range lines {
		if n, err := r.line(l); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", file, n, err)
		}
	}
	if n, err := r.finish(0); err != nil {
		return nil, fmt.Errorf("%s:%d: %w", file, n, err)
	}
	return r.sets, nil
}

// setsReader reads a file's lines into its sets.
type setsReader struct {
	sets []Set
	cur  *setBuilder
}

func newSetBuilder(n int) *setBuilder {
	return &setBuilder{constrained: map[string]int{}, n: n}
}

// line reads one line; an error comes with the line it is about.
func (r *setsReader) line(l line) (int, error) {
	var err error
	switch {
	case strings.HasPrefix(l.key, "["):
		if l.key != sectionSet {
			return l.n, fmt.Errorf("%q: the only section is [set]", l.key)
		}
		return r.finish(l.n)
	case l.key == keyTool || l.key == keyToolUnconstrained:
		err = r.cur.addTool(l.value, l.key == keyTool, l.n)
	case l.key == keyConstrain:
		err = r.cur.constrain(l.value)
	default:
		err = fmt.Errorf("unknown key %q", l.key)
	}
	return l.n, err
}

// finish ends the current set and starts one at line next: a [set] with no
// tool is an error, the lines before the first [set] may be none. So is a
// tool = line no constrain line reached (one after the set's constrain =
// * lines, or in a set with none): it would allow the tool with any
// arguments, which only tool_unconstrained may say.
func (r *setsReader) finish(next int) (int, error) {
	for _, t := range r.cur.set.Tools {
		if n := r.cur.constrained[t.Name]; n > 0 && len(t.Constraints) == 0 {
			return n, fmt.Errorf("tool %s: no constrain line of its set reaches it (constraints follow their tools); to allow it with any arguments, write tool_unconstrained = %s", t.Name, t.Name)
		}
	}
	switch {
	case len(r.cur.set.Tools) > 0:
		r.sets = append(r.sets, r.cur.set)
	case r.cur.n > 0:
		return r.cur.n, errors.New("an empty set")
	}
	r.cur = newSetBuilder(next)
	return 0, nil
}

func (b *setBuilder) addTool(name string, constrainable bool, n int) error {
	if !toolName.MatchString(name) {
		return fmt.Errorf("%q is not a tool name", name)
	}
	for _, t := range b.set.Tools {
		if t.Name == name {
			return fmt.Errorf("%s is listed twice in its set", name)
		}
	}
	b.set.Tools = append(b.set.Tools, Tool{Name: name, Unconstrained: !constrainable})
	if constrainable {
		b.constrained[name] = n
	}
	return nil
}

// constrain adds `<tool or *> <path> = | ^= <value>` to the set's tools.
func (b *setBuilder) constrain(value string) error {
	f := strings.Fields(value)
	if len(f) != 4 || (f[2] != "=" && f[2] != "^=") {
		return fmt.Errorf("constrain %q is not `<tool or *> arguments.<path> = | ^= <value>`", value)
	}
	target, path, op, v := f[0], f[1], f[2], f[3]
	if !argumentPath.MatchString(path) {
		return fmt.Errorf("constrain path %q is not arguments.<key>[.<key>...]", path)
	}
	c := Constraint{Path: strings.Split(path, "."), Value: v, Prefix: op == "^="}
	matched := false
	for i, t := range b.set.Tools {
		if b.constrained[t.Name] > 0 && (target == "*" || target == t.Name) {
			b.set.Tools[i].Constraints = append(b.set.Tools[i].Constraints, c)
			matched = true
		}
	}
	if !matched {
		if target == "*" {
			return errors.New("constrain *: no tool = line in its set before it")
		}
		return fmt.Errorf("%q is no tool = line of its set (constraints follow their tools)", target)
	}
	return nil
}
