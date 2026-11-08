package boundaries

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/ayeaaaa/mix2api-v2"

// TestDependencyDirection enforces the design's root import boundaries:
//   - internal/core never imports providers, api, or web;
//   - internal/api never imports a concrete provider package.
func TestDependencyDirection(t *testing.T) {
	root := repositoryRoot(t)
	cases := []struct {
		name      string
		dir       string
		forbidden func(string) bool
	}{
		{
			name: "core",
			dir:  filepath.Join(root, "internal", "core"),
			forbidden: func(importPath string) bool {
				for _, prefix := range []string{
					modulePath + "/internal/providers",
					modulePath + "/internal/api",
					modulePath + "/internal/web",
				} {
					if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
						return true
					}
				}
				return false
			},
		},
		{
			name: "api",
			dir:  filepath.Join(root, "internal", "api"),
			forbidden: func(importPath string) bool {
				return strings.HasPrefix(importPath, modulePath+"/internal/providers/")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := filepath.WalkDir(tc.dir, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
					return nil
				}
				file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
				if err != nil {
					return err
				}
				for _, importSpec := range file.Imports {
					importPath, err := strconv.Unquote(importSpec.Path.Value)
					if err != nil {
						continue
					}
					if tc.forbidden(importPath) {
						relative, _ := filepath.Rel(root, path)
						t.Errorf("%s imports forbidden package %s", relative, importPath)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("scan %s: %v", tc.dir, err)
			}
		})
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root %q has no go.mod: %v", root, err)
	}
	return root
}
