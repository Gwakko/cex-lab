package archtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/gwakko/cex-lab"

// modules are the business modules. Each lives in internal/<name>, may have
// adapters under internal/<name>/adapters and private code under
// internal/<name>/internal.
var modules = []string{"ledger", "trading", "custody"}

// forbiddenInDomain are standard packages domain code must not use: I/O and
// networking belong in adapters or the composition root.
var forbiddenInDomain = []string{"os", "os/exec", "net", "net/http"}

// apiFiles may declare exported top-level identifiers in a module root.
var apiFiles = []string{"api.go", "ports.go"}

type pkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Imports    []string // production imports only; test imports are not checked
}

type kind int

const (
	kindOther kind = iota
	kindEvents
	kindPlatform
	kindModuleDomain
	kindModuleAdapter
	kindApp
	kindCmd
)

type class struct {
	kind   kind
	module string // for module kinds
}

func classify(path string) class {
	rel, ok := strings.CutPrefix(path, module+"/")
	if !ok {
		return class{kind: kindOther}
	}
	switch {
	case rel == "internal/events" || strings.HasPrefix(rel, "internal/events/"):
		return class{kind: kindEvents}
	case strings.HasPrefix(rel, "internal/platform/"):
		return class{kind: kindPlatform}
	case rel == "internal/app" || strings.HasPrefix(rel, "internal/app/"):
		return class{kind: kindApp}
	case strings.HasPrefix(rel, "cmd/"):
		return class{kind: kindCmd}
	}
	for _, m := range modules {
		root := "internal/" + m
		if rel == root || strings.HasPrefix(rel, root+"/") {
			if strings.HasPrefix(rel, root+"/adapters/") {
				return class{kind: kindModuleAdapter, module: m}
			}
			return class{kind: kindModuleDomain, module: m}
		}
	}
	return class{kind: kindOther}
}

func short(path string) string { return strings.TrimPrefix(path, module+"/") }

// importViolations checks every production import against the rules.
func importViolations(pkgs []pkg) []string {
	var out []string
	for _, p := range pkgs {
		from := classify(p.ImportPath)
		for _, imp := range p.Imports {
			if msg := rule(from, imp); msg != "" {
				out = append(out, fmt.Sprintf("%s imports %s: %s", short(p.ImportPath), short(imp), msg))
			}
		}
	}
	return out
}

func rule(from class, imp string) string {
	to := classify(imp)
	local := strings.HasPrefix(imp, module+"/")
	switch from.kind {
	case kindEvents:
		if local {
			return "internal/events is a shared contract and must import nothing from the repo"
		}
	case kindPlatform:
		if to.kind == kindModuleDomain || to.kind == kindModuleAdapter || to.kind == kindApp || to.kind == kindCmd {
			return "platform code must not depend on modules, app or cmd"
		}
	case kindModuleDomain:
		if slices.Contains(forbiddenInDomain, imp) {
			return fmt.Sprintf("module %s domain code must not do I/O; move it to an adapter", from.module)
		}
		return moduleRule(from, to, imp, false)
	case kindModuleAdapter:
		return moduleRule(from, to, imp, true)
	}
	return ""
}

func moduleRule(from, to class, imp string, adapter bool) string {
	switch to.kind {
	case kindModuleDomain, kindModuleAdapter:
		if to.module != from.module {
			return fmt.Sprintf("module %s must not import module %s; define a port in %s and wire it in internal/app",
				from.module, to.module, from.module)
		}
		if to.kind == kindModuleAdapter && !adapter {
			return fmt.Sprintf("module %s must not import its own adapters; internal/app injects them", from.module)
		}
	case kindApp, kindCmd:
		return "modules must not import the composition root or commands"
	case kindPlatform:
		if !adapter && imp != module+"/internal/platform/trace" {
			return fmt.Sprintf("module %s domain code may only use internal/platform/trace; take other infrastructure through a port", from.module)
		}
	}
	return ""
}

// apiViolations checks that module root packages declare exported top-level
// identifiers only in api.go and ports.go.
func apiViolations(pkgs []pkg) ([]string, error) {
	var out []string
	for _, p := range pkgs {
		if !isModuleRoot(p.ImportPath) {
			continue
		}
		for _, f := range p.GoFiles {
			if slices.Contains(apiFiles, f) {
				continue
			}
			names, err := exportedDecls(filepath.Join(p.Dir, f))
			if err != nil {
				return nil, err
			}
			for _, n := range names {
				out = append(out, fmt.Sprintf("%s/%s exports %s: a module's public API belongs in api.go or ports.go", short(p.ImportPath), f, n))
			}
		}
	}
	return out, nil
}

func isModuleRoot(path string) bool {
	for _, m := range modules {
		if path == module+"/internal/"+m {
			return true
		}
	}
	return false
}

func exportedDecls(file string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.IsExported() {
				names = append(names, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						names = append(names, s.Name.Name)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.IsExported() {
							names = append(names, n.Name)
						}
					}
				}
			}
		}
	}
	return names, nil
}

func listPackages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", module+"/...")
	cmd.Dir = "../.."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) < 20 {
		t.Fatalf("go list returned only %d packages; is the test running from the repo?", len(pkgs))
	}
	return pkgs
}

func TestDependencyRules(t *testing.T) {
	for _, v := range importViolations(listPackages(t)) {
		t.Error(v)
	}
}

func TestModulePublicAPIIsInAPIFiles(t *testing.T) {
	vs, err := apiViolations(listPackages(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		t.Error(v)
	}
}

// TestRulesCatchViolations proves the checker bites: each synthetic package
// breaks one rule and must be reported.
func TestRulesCatchViolations(t *testing.T) {
	p := func(path string, imports ...string) pkg {
		for i, imp := range imports {
			if strings.HasPrefix(imp, "internal/") || strings.HasPrefix(imp, "cmd/") {
				imports[i] = module + "/" + imp
			}
		}
		return pkg{ImportPath: module + "/" + path, Imports: imports}
	}
	cases := []struct {
		name string
		pkg  pkg
		want string
	}{
		{"trading imports ledger", p("internal/trading/internal/service", "internal/ledger"), "must not import module ledger"},
		{"custody imports trading", p("internal/custody/internal/watch", "internal/trading"), "must not import module trading"},
		{"custody imports ledger", p("internal/custody", "internal/ledger"), "must not import module ledger"},
		{"ledger domain uses os", p("internal/ledger/internal/core", "os"), "must not do I/O"},
		{"domain uses net/http", p("internal/trading/internal/engine", "net/http"), "must not do I/O"},
		{"domain uses telemetry", p("internal/ledger/internal/core", "internal/platform/telemetry"), "may only use internal/platform/trace"},
		{"domain uses journal", p("internal/trading/internal/engine", "internal/platform/journal"), "may only use internal/platform/trace"},
		{"module imports own adapter", p("internal/ledger", "internal/ledger/adapters/memstore"), "must not import its own adapters"},
		{"adapter imports other module", p("internal/trading/adapters/filelog", "internal/ledger"), "must not import module ledger"},
		{"module imports app", p("internal/custody/internal/watch", "internal/app/instrument"), "composition root"},
		{"events imports a module", p("internal/events", "internal/ledger"), "shared contract"},
		{"platform imports a module", p("internal/platform/outbox", "internal/ledger"), "platform code must not depend"},
	}
	for _, tc := range cases {
		vs := importViolations([]pkg{tc.pkg})
		if len(vs) != 1 || !strings.Contains(vs[0], tc.want) {
			t.Errorf("%s: got %q, want one violation containing %q", tc.name, vs, tc.want)
		}
	}

	allowed := []pkg{
		p("internal/trading/internal/service", "context", "internal/platform/trace", "internal/trading/internal/engine"),
		p("internal/ledger/adapters/filestore", "internal/ledger", "internal/platform/journal", "os"),
		p("internal/app", "internal/ledger", "internal/trading", "internal/custody/adapters/fakechain"),
		p("internal/platform/outbox", "internal/events", "log/slog"),
		p("cmd/demo", "internal/app", "internal/ledger"),
	}
	if vs := importViolations(allowed); len(vs) != 0 {
		t.Errorf("allowed imports reported: %q", vs)
	}
}
