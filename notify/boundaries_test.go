package notify

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Notices are one feature and bills another: neither may import the other. Bills asks whether orders
// are stopped through platform.Gate, which main wires to the notices. If this fails, the new import
// belongs behind that gate (or in platform), not here.
func TestNotifyDoesNotDependOnBills(t *testing.T) {
	forbidden := []string{"Bills-Backend/order", "Bills-Backend/abs", "Bills-Backend/kora", "Bills-Backend/monnify", "Bills-Backend/email"}
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad {
					t.Errorf("%s imports %s: notices must not depend on bills", f, path)
				}
			}
		}
	}
}
