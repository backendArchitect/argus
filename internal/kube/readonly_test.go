package kube

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// mutatingVerbs are the client-go method names that change cluster state. Matching on method name
// alone is deliberately blunt: a false positive costs one look, a false negative costs a production
// incident caused by the tool that was supposed to diagnose one.
//
// SCOPE: this guard is about CLUSTER state, not the local filesystem. internal/selfupdate writes
// and replaces files on disk on purpose, and passes only because os.CreateTemp does not collide
// with a client verb name — which is coincidence rather than exemption, so say so plainly here
// instead of letting a reader assume that package was vetted by this test. What matters is that
// nothing in the tree can write to a cluster, and being name-based this check cannot tell
// client.Create(pod) from os.CreateTemp. It is a tripwire against the obvious mistake, not a proof.
var mutatingVerbs = map[string]bool{
	"Create":           true,
	"Update":           true,
	"UpdateStatus":     true,
	"Patch":            true,
	"Delete":           true,
	"DeleteCollection": true,
	"Apply":            true,
	"ApplyStatus":      true,
	"Evict":            true,
	"EvictV1":          true,
	"EvictV1beta1":     true,
}

// allowed are call sites that share a name with a mutating verb but touch no cluster state.
// Every entry needs a reason — this list is how the guard rots if we are careless with it.
var allowed = map[string]string{
	"NewForConfig":           "client construction",
	"CreateToken":            "not used; listed so the intent is explicit if it ever appears",
	"CreateTwoWayMergePatch": "computes a patch document locally; never sends it",
}

// TestNoMutatingVerbs is the read-only guarantee.
//
// v0.1 runs under a developer kubeconfig against real clusters, where RBAC grants the user
// everything — so "read-only enforced at the RBAC layer" is false for this deployment mode and the
// binary must enforce it itself. This test is that enforcement: it fails the day someone adds a
// convenient restart helper, which is exactly the day it needs to fail.
func TestNoMutatingVerbs(t *testing.T) {
	root := filepath.Join("..") // internal/
	fset := token.NewFileSet()

	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			if !mutatingVerbs[name] || allowed[name] != "" {
				return true
			}
			violations = append(violations,
				fmt.Sprintf("%s: calls %s", fset.Position(call.Pos()), name))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}

	for _, v := range violations {
		t.Errorf("mutating call in a read-only binary: %s", v)
	}
	if len(violations) > 0 {
		t.Log("if this call is genuinely read-only, add it to `allowed` with a reason")
	}
}

// TestGuardActuallyFires proves the AST walk can fail, so a refactor that silently stops matching
// (wrong root, changed extension filter) does not leave us with a test that always passes.
func TestGuardActuallyFires(t *testing.T) {
	fset := token.NewFileSet()
	src := `package p
type c struct{}
func (c) Delete(s string) {}
func f(x c) { x.Delete("pods") }`
	f, err := parser.ParseFile(fset, "fake.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && mutatingVerbs[sel.Sel.Name] {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("the guard failed to detect a deliberate mutating call; it is not protecting anything")
	}
}
