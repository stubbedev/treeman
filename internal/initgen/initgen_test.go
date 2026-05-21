package initgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/schema"
)

func TestRenderTemplateContainsModelineAndRepoName(t *testing.T) {
	dir := t.TempDir()
	os.Rename(dir, dir) // no-op — just exercising the path
	body := RenderTemplate(dir)
	if !strings.Contains(body, "$schema="+schema.URL) {
		t.Errorf("missing schema modeline:\n%s", body)
	}
	want := "name: " + filepath.Base(dir)
	if !strings.Contains(body, want) {
		t.Errorf("missing %q:\n%s", want, body)
	}
}

func TestRenderTemplateEmitsGroupForGoMod(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module foo\n"), 0o644)
	body := RenderTemplate(dir)
	if !strings.Contains(body, "go mod download") {
		t.Errorf("missing go mod download hook:\n%s", body)
	}
}

func TestRenderTemplateEmitsNpmGroup(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}\n"), 0o644)
	body := RenderTemplate(dir)
	if !strings.Contains(body, "npm ci") {
		t.Errorf("missing npm ci hook:\n%s", body)
	}
}

func TestRenderTemplateParsesAsYAML(t *testing.T) {
	dir := t.TempDir()
	body := RenderTemplate(dir)
	var doc any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("rendered template is not valid YAML: %v\n%s", err, body)
	}
}

func TestWriteYAMLRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".treeman.yaml")
	os.WriteFile(target, []byte("existing\n"), 0o644)
	_, _, _, err := WriteYAML(dir, false)
	if err == nil {
		t.Error("WriteYAML(force=false) should error when file exists")
	}
}

func TestWriteYAMLOverwritesWithForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".treeman.yaml")
	os.WriteFile(target, []byte("existing\n"), 0o644)
	_, created, body, err := WriteYAML(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("created=true when overwriting existing file")
	}
	if body == "existing\n" {
		t.Error("file not overwritten")
	}
}

func TestDetectJSPkgMgrPrecedence(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "yarn.lock"), []byte("\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte("\n"), 0o644)
	// pnpm beats yarn
	if got := detectJSPkgMgr(dir); got != "pnpm" {
		t.Errorf("detectJSPkgMgr = %q, want pnpm", got)
	}
}
