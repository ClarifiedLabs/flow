package flow_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const testenvImportPath = "github.com/ClarifiedLabs/flow/internal/testenv"

// TestAllTestPackagesRouteThroughTestenv fails when a directory containing
// _test.go files lacks a TestMain that calls testenv.Main or testenv.Isolate,
// so new test packages cannot silently lose hermetic isolation.
func TestAllTestPackagesRouteThroughTestenv(t *testing.T) {
	testDirs := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != "." && (strings.HasPrefix(name, ".") || name == "bin" || name == "node_modules" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			dir := filepath.Dir(path)
			testDirs[dir] = append(testDirs[dir], path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}

	for dir, files := range testDirs {
		if dir == filepath.Join("internal", "testenv") {
			continue // testenv's own tests may call Main without the import
		}
		if !dirHasTestenvTestMain(t, files) {
			t.Errorf("%s: no TestMain routes through %s; add a testmain_test.go with\n\nfunc TestMain(m *testing.M) { testenv.Main(m) }", dir, testenvImportPath)
		}
	}
}

func dirHasTestenvTestMain(t *testing.T, files []string) bool {
	t.Helper()
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		testenvName := ""
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) != testenvImportPath {
				continue
			}
			testenvName = "testenv"
			if imp.Name != nil {
				testenvName = imp.Name.Name
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
				continue
			}
			if testenvName == "" {
				return false // a TestMain exists but ignores testenv
			}
			isolates := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if ok && ident.Name == testenvName && (sel.Sel.Name == "Main" || sel.Sel.Name == "Isolate") {
					isolates = true
				}
				return true
			})
			return isolates
		}
	}
	return false
}
