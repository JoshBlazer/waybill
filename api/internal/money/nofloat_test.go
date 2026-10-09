package money

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moneyPackages are the directories (relative to this one) that handle
// amounts. Add a package here when it starts handling money.
var moneyPackages = []string{".", "../ledger", "../statemachine"}

// TestNoFloatsInMoneyPackages fails if any non-test source file in a money
// package mentions a floating-point identifier: float32, float64, big.Float,
// strconv.ParseFloat, pgtype's Float64Value, and so on. It inspects the
// syntax tree, so comments and strings do not trigger it.
func TestNoFloatsInMoneyPackages(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range moneyPackages {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && strings.Contains(strings.ToLower(id.Name), "float") {
					t.Errorf("%s: floating-point identifier %q in a money package", fset.Position(id.Pos()), id.Name)
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("no files checked; moneyPackages paths are wrong")
	}
}
