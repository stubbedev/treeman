package patcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/template"
)

func TestPatchYAMLFile_SetsNestedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := "database:\n  host: localhost  # keep this comment\n  name: app\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchYAMLFile(path, map[string]string{
		"database.name": "app_feature-x",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	var doc map[string]any
	if err := yaml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	db := doc["database"].(map[string]any)
	if db["name"] != "app_feature-x" {
		t.Errorf("name = %v, want app_feature-x", db["name"])
	}
	if !strings.Contains(string(got), "keep this comment") {
		t.Errorf("comment lost:\n%s", got)
	}
}

func TestPatchYAMLFile_MissingReturnsMissing(t *testing.T) {
	out, err := PatchYAMLFile(filepath.Join(t.TempDir(), "nope.yaml"), map[string]string{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if out != Missing {
		t.Errorf("outcome = %v, want Missing", out)
	}
}

func TestPatchJSONFile_SetsNestedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tsconfig.json")
	original := `{"compilerOptions":{"outDir":"dist"}}` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchJSONFile(path, map[string]string{
		"compilerOptions.outDir": "dist/feature-x",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("json: %v", err)
	}
	co := doc["compilerOptions"].(map[string]any)
	if co["outDir"] != "dist/feature-x" {
		t.Errorf("outDir = %v, want dist/feature-x", co["outDir"])
	}
}

func TestPatchJSONFile_NumericValueBecomesNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "redis.json")
	if err := os.WriteFile(path, []byte(`{"db":0}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchJSONFile(path, map[string]string{"db": "7"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["db"].(float64) != 7 {
		t.Errorf("db = %v, want 7 (numeric)", doc["db"])
	}
}

func TestApply_AutoDetectsDotenv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DB=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := config.Patch{
		File: ".env",
		Set:  map[string]string{"DB": "app_{slug}"},
	}
	tplCtx := template.Context{Slug: "feature-x", SlugDash: "feature-x"}
	res, err := Apply(patch, dir, tplCtx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Driver != "dotenv" || res.Outcome != Updated {
		t.Errorf("res = %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.Contains(string(got), "DB=app_feature-x") {
		t.Errorf("not patched: %s", got)
	}
}

func TestApply_AutoDetectsYAMLAndJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.yaml"), []byte("database:\n  name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.json"), []byte(`{"database":{"name":"x"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tplCtx := template.Context{Slug: "f-x", SlugDash: "f-x"}
	for _, file := range []string{"c.yaml", "c.json"} {
		patch := config.Patch{File: file, Set: map[string]string{"database.name": "app_{slug}"}}
		res, err := Apply(patch, dir, tplCtx)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if res.Outcome != Updated {
			t.Errorf("%s: outcome = %v", file, res.Outcome)
		}
	}
}

func TestApply_AutoDetectsPHPUnitXML(t *testing.T) {
	dir := t.TempDir()
	body := `<phpunit><php></php></phpunit>`
	if err := os.WriteFile(filepath.Join(dir, "phpunit.xml.dist"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := config.Patch{File: "phpunit.xml.dist", Set: map[string]string{"DB_DATABASE": "app_{slug}"}}
	res, err := Apply(patch, dir, template.Context{Slug: "f-x", SlugDash: "f-x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Driver != "phpunit" || res.Outcome != Updated {
		t.Errorf("res = %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "phpunit.xml.dist"))
	if !strings.Contains(string(got), `name="DB_DATABASE" value="app_f-x"`) {
		t.Errorf("not patched: %s", got)
	}
}

func TestApply_ExplicitFormatOverridesExtension(t *testing.T) {
	dir := t.TempDir()
	// `.txt` has no auto-detect; force `dotenv`.
	if err := os.WriteFile(filepath.Join(dir, "settings.txt"), []byte("X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := config.Patch{File: "settings.txt", Format: "dotenv", Set: map[string]string{"X": "app_{slug}"}}
	res, err := Apply(patch, dir, template.Context{Slug: "f-x", SlugDash: "f-x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Driver != "dotenv" || res.Outcome != Updated {
		t.Errorf("res = %+v", res)
	}
}

func TestApply_RejectsEmptySet(t *testing.T) {
	dir := t.TempDir()
	patch := config.Patch{File: ".env"}
	_, err := Apply(patch, dir, template.Context{Slug: "s", SlugDash: "s"})
	if err == nil || !strings.Contains(err.Error(), "set:") {
		t.Errorf("expected empty-set error, got %v", err)
	}
}

func TestApply_RejectsUnknownExtension(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := config.Patch{File: "settings.txt", Set: map[string]string{"X": "y"}}
	_, err := Apply(patch, dir, template.Context{Slug: "s", SlugDash: "s"})
	if err == nil || !strings.Contains(err.Error(), "cannot infer format") {
		t.Errorf("expected unknown-extension error, got %v", err)
	}
}
