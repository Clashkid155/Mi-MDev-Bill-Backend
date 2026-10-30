package platform

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shared package must not depend on any feature: bills (order) and notifications (notify) build
// on it, never the other way round. If this fails, the new import belongs in the feature, or what
// it needs belongs here.
func TestPlatformDependsOnNoFeature(t *testing.T) {
	forbidden := []string{"Bills-Backend/order", "Bills-Backend/notify", "Bills-Backend/abs", "Bills-Backend/kora", "Bills-Backend/monnify", "Bills-Backend/email"}
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
					t.Errorf("%s imports %s: the shared package must not depend on a feature", f, path)
				}
			}
		}
	}
}
