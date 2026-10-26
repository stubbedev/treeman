package harness_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"testing"
)

const harnessPkg = "github.com/stubbedev/treeman/e2e/harness"

func TestEveryE2ESuiteImportsHarness(t *testing.T) {
	cmd := exec.Command(
		"go",
		"list",
		"-tags=e2e",
		"-json=ImportPath,Imports,TestGoFiles,TestImports,XTestGoFiles,XTestImports",
		"./e2e/...",
	)
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./e2e/...: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath                         string
			Imports, TestImports, XTestImports []string
			TestGoFiles, XTestGoFiles          []string
		}
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		if p.ImportPath == harnessPkg || len(p.TestGoFiles)+len(p.XTestGoFiles) == 0 {
			continue
		}
		if !slices.Contains(p.Imports, harnessPkg) && !slices.Contains(p.TestImports, harnessPkg) &&
			!slices.Contains(p.XTestImports, harnessPkg) {
			t.Errorf("%s does not import %s, so its tests skip the sandbox guard", p.ImportPath, harnessPkg)
		}
	}
}
