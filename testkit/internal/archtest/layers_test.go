// Package archtest enforces the layering rule of TestKit:
// core <- adapters <- steps <- (scenarios: YAML only). A lower layer never
// imports a higher one; only cmd/ wires everything together.
package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const module = "github.com/tuannm99/testkit/testkit/"

var rank = map[string]int{"core": 0, "adapters": 1, "steps": 2}

func TestLayering(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	checked := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		layer := strings.Split(filepath.ToSlash(rel), "/")[0]
		from, ok := rank[layer]
		if !ok {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, module) {
				continue
			}
			target := strings.Split(strings.TrimPrefix(p, module), "/")[0]
			if target == "cmd" {
				t.Errorf("%s imports %s: nothing may import cmd/", rel, p)
			}
			if to, ok := rank[target]; ok && to > from {
				t.Errorf("%s (layer %s) imports %s (layer %s): lower layers must not depend on higher ones", rel, layer, p, target)
			}
			checked++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no internal imports checked; is the walker broken?")
	}
}
