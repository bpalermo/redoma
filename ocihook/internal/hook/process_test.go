package hook

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bpalermo/redoma/common/register"
)

// agentProcess is the process of an agent as Podman 4.9 writes it for
// --user 1000:1000 --cap-drop all --security-opt no-new-privileges, the
// options README.md gives.
const agentProcess = `{"user":{"uid":1000,"gid":1000,"additionalGids":[1000]},"noNewPrivileges":true,` +
	`"capabilities":{"bounding":[],"effective":[],"inheritable":[],"permitted":[],"ambient":[]}}`

// create runs createRuntime on a spec of process and mounts, and returns
// its error, checking that a refusal mounted and asked nothing.
func create(t *testing.T, process, mounts string) error {
	t.Helper()
	cp := &fakeCP{answer: func(register.Request) string { return "" }}
	m := &fakeMounter{mounted: map[string]int{}}
	h, _ := newHook(cp, m)
	st := state(t, true)
	cfg := `{"ociVersion":"1.2.0","root":{"path":"rootfs"}`
	if process != "" {
		cfg += `,"process":` + process
	}
	if mounts != "" {
		cfg += `,"mounts":` + mounts
	}
	cfg += `,"linux":{"namespaces":` + fresh + `}}`
	st.Bundle = writeBundle(t, cfg)
	err := h.CreateRuntime(t.Context(), st)
	if err != nil && (len(m.mounted) != 0 || len(cp.requests) != 0) {
		t.Errorf("refused (%v), but mounts %v, requests %v", err, m.mounted, cp.requests)
	}
	return err
}

func expect(t *testing.T, name string, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: %v, want accepted", name, err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Errorf("%s: %v, want %q", name, err, want)
	}
}

func proc(user, nnp, caps string) string {
	return `{"user":` + user + `,"noNewPrivileges":` + nnp + `,"capabilities":` + caps + `}`
}

const noCaps = `{"bounding":[],"effective":[],"inheritable":[],"permitted":[],"ambient":[]}`

// TestCreateRuntimeChecksTheProcess: an agent runs as a user that is no
// host service, cannot gain privileges, and holds no capability past
// AllowedCaps in any set; anything else could become a service or open
// its sockets. The launcher's options pass.
func TestCreateRuntimeChecksTheProcess(t *testing.T) {
	user := `{"uid":1000,"gid":1000}`
	for name, tc := range map[string]struct {
		process string
		want    string
	}{
		"the launcher's options":  {agentProcess, ""},
		"no capabilities object":  {proc(user, "true", `{}`), ""},
		"a harmless capability":   {proc(user, "true", `{"bounding":["CAP_KILL"],"effective":["CAP_KILL"]}`), ""},
		"no process":              {"", "runs as root"},
		"no process.user":         {`{"noNewPrivileges":true}`, "runs as root"},
		"uid 0":                   {proc(`{"uid":0,"gid":1000}`, "true", noCaps), "runs as root"},
		"no no-new-privileges":    {proc(user, "false", noCaps), "no-new-privileges"},
		"noNewPrivileges missing": {`{"user":{"uid":1000,"gid":1000}}`, "no-new-privileges"},
		"--group-add redoma-lead": {proc(`{"uid":1000,"gid":1000,"additionalGids":[1000,964]}`, "true", noCaps), "gid 964 (redoma-lead"},
		"--user 1000:964":         {proc(`{"uid":1000,"gid":964}`, "true", noCaps), "gid 964 (redoma-lead"},
		"--group-add redoma-sds":  {proc(`{"uid":1000,"gid":1000,"additionalGids":[961]}`, "true", noCaps), "gid 961 (redoma-sds"},
		"--user redoma-relay":     {proc(`{"uid":963,"gid":1000}`, "true", noCaps), "uid 963 (redoma-relay"},
		"gid 0":                   {proc(`{"uid":1000,"gid":0}`, "true", noCaps), "gid 0"},
		"--group-add root":        {proc(`{"uid":1000,"gid":1000,"additionalGids":[0]}`, "true", noCaps), "gid 0"},
		"--user 961":              {proc(`{"uid":961,"gid":1000}`, "true", noCaps), "uid 961 (redoma-sds"},
		"a gid of another type":   {proc(`{"uid":1000,"gid":"964"}`, "true", noCaps), "process.user.gid"},
		"User in another case":    {`{"User":{"uid":1000},"noNewPrivileges":true}`, "in another case"},
	} {
		expect(t, name, create(t, tc.process, ""), tc.want)
	}
	for _, c := range []string{"CAP_SETUID", "CAP_SETGID", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER", "CAP_CHOWN", "CAP_SYS_ADMIN", "CAP_SYS_PTRACE", "CAP_MKNOD"} {
		for _, set := range []string{"bounding", "effective", "inheritable", "permitted", "ambient"} {
			caps := `{"` + set + `":["CAP_KILL","` + c + `"]}`
			expect(t, c+" in "+set, create(t, proc(user, "true", caps), ""), c+" in its "+set)
		}
	}
}

// readKeys are the keys the package's sources read from a spec, found in
// them: the key of field and stringField, of an index by a string literal,
// of a map[string]any literal (the key → destination tables), and of a
// []string literal ranged over (the capability sets).
func readKeys(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	lit := func(e ast.Expr) {
		if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
			if s, err := strconv.Unquote(b.Value); err == nil {
				seen[s] = true
			}
		}
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && (id.Name == "field" || id.Name == "stringField") && len(n.Args) > 1 {
					lit(n.Args[1])
				}
			case *ast.IndexExpr:
				lit(n.Index)
			case *ast.RangeStmt:
				ranged(n, lit)
			}
			return true
		})
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ranged gives lit the keys of a literal ranged over whose range variable
// is a field key in the loop: a map literal's keys (the key → destination
// tables), a slice literal's elements (the capability sets).
func ranged(r *ast.RangeStmt, lit func(ast.Expr)) {
	c, ok := r.X.(*ast.CompositeLit)
	if !ok {
		return
	}
	v := r.Value
	if _, isMap := c.Type.(*ast.MapType); isMap {
		v = r.Key
	}
	id, ok := v.(*ast.Ident)
	if !ok {
		return
	}
	used := false
	ast.Inspect(r.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) > 1 {
			fn, _ := call.Fun.(*ast.Ident)
			arg, _ := call.Args[1].(*ast.Ident)
			used = used || fn != nil && arg != nil && (fn.Name == "field" || fn.Name == "stringField") && arg.Name == id.Name
		}
		return true
	})
	if !used {
		return
	}
	for _, e := range c.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			e = kv.Key
		}
		lit(e)
	}
}

// TestSpecKeysCoverWhatIsRead: every key the hook reads is in specKeys,
// or a key of it in another case gets past the case check (runc's
// encoding/json would take it, the hook would not see it).
func TestSpecKeysCoverWhatIsRead(t *testing.T) {
	keys := readKeys(t)
	for _, k := range keys {
		if !slices.Contains(specKeys, k) {
			t.Errorf("%q is read but not in specKeys", k)
		}
	}
	// The scan finds what it must, the reads of every kind: field's,
	// stringField's, an index's, a table's and a ranged list's.
	for _, k := range []string{"mounts", "path", "linux", "allow", "minor", "options", "uid", "ambient"} {
		if !slices.Contains(keys, k) {
			t.Errorf("the scan of the sources misses %q (it found %v)", k, keys)
		}
	}
}
