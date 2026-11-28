package authz_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// R5 / EST-9: every action policy-svc asks authorization-svc about is
// registered in access-control-svc's permission registry by a migration.
// An unregistered action can never be granted, so the route behind it would
// answer 403 to everyone, and nobody would notice until it was used.
func TestR5_EveryCheckedActionIsRegistered(t *testing.T) {
	actions := checkedActions(t)
	if len(actions) < 20 {
		t.Fatalf("found only %d action constants in client.go: the parser is looking in the wrong place", len(actions))
	}
	ups, err := filepath.Glob("../../../access-control-svc/deployments/migrations/*.up.sql")
	if err != nil || len(ups) == 0 {
		t.Skip("access-control-svc migrations are not beside this service (not the monorepo checkout)")
	}
	var registry strings.Builder
	for _, f := range ups {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		registry.Write(b)
	}
	text := registry.String()
	for _, a := range actions {
		if !strings.Contains(text, "('"+a+"'") {
			t.Errorf("%s is checked by policy-svc but registered by no access-control-svc migration", a)
		}
	}
}

// checkedActions reads the Action* string constants from client.go.
func checkedActions(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "client.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Action") || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					out = append(out, v)
				}
			}
		}
	}
	return out
}
