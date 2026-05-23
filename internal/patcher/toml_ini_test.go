package patcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/template"
)

func TestPatchTOMLFile_SetsNestedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pyproject.toml")
	original := "[tool.poetry]\nname = \"app\"\nversion = \"0.1.0\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchTOMLFile(path, map[string]string{
		"tool.poetry.name": "app_feature-x",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	var doc map[string]any
	if err := toml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("toml: %v", err)
	}
	pkg := doc["tool"].(map[string]any)["poetry"].(map[string]any)
	if pkg["name"] != "app_feature-x" {
		t.Errorf("name = %v, want app_feature-x", pkg["name"])
	}
	// version preserved
	if pkg["version"] != "0.1.0" {
		t.Errorf("version lost: %v", pkg["version"])
	}
}

func TestPatchTOMLFile_NumericValueBecomesInt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("port = 5432\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchTOMLFile(path, map[string]string{"port": "6432"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	var doc map[string]any
	_ = toml.Unmarshal(got, &doc)
	if doc["port"].(int64) != 6432 {
		t.Errorf("port = %v, want 6432 (int)", doc["port"])
	}
}

func TestPatchINIFile_SetsKeyAndPreservesComment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	original := "; top comment\n[database]\nhost = localhost\nport = 5432\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchINIFile(path, map[string]string{
		"database.host": "db-feature-x",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "host = db-feature-x") {
		t.Errorf("host not set: %s", got)
	}
	if !strings.Contains(string(got), "; top comment") {
		t.Errorf("comment lost: %s", got)
	}
	if !strings.Contains(string(got), "port = 5432") {
		t.Errorf("sibling key lost: %s", got)
	}
}

func TestPatchINIFile_TopLevelKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	if err := os.WriteFile(path, []byte("debug = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchINIFile(path, map[string]string{"debug": "true"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "debug = true") {
		t.Errorf("debug not set: %s", got)
	}
}

func TestPatchINIFile_RejectsDeepPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := PatchINIFile(path, map[string]string{"a.b.c": "v"})
	if err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Errorf("expected deep-path error, got %v", err)
	}
}

func TestApply_AutoDetectsTOMLAndINI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`[tool.poetry]`+"\nname = \"app\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.ini"), []byte("[db]\nname = app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tplCtx := template.Context{Slug: "f-x", SlugDash: "f-x"}

	for _, tc := range []struct {
		file, key, wantDriver string
	}{
		{"pyproject.toml", "tool.poetry.name", "toml"},
		{"config.ini", "db.name", "ini"},
	} {
		patch := config.Patch{
			File: tc.file,
			Set:  map[string]string{tc.key: "app_{slug}"},
		}
		res, err := Apply(patch, dir, tplCtx)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if res.Driver != tc.wantDriver {
			t.Errorf("%s: driver = %s, want %s", tc.file, res.Driver, tc.wantDriver)
		}
		if res.Outcome != Updated {
			t.Errorf("%s: outcome = %v", tc.file, res.Outcome)
		}
	}
}
