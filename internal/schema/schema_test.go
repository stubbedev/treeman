package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderProducesObjectSchema(t *testing.T) {
	b, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("rendered schema is not valid JSON: %v", err)
	}
	if s["$schema"] == nil {
		t.Errorf("missing $schema declaration")
	}
}

func TestSetModelineInsertsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, ".treeman.yaml")
	body := "repo:\n  name: foo\n"
	if err := os.WriteFile(repo, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := SetModeline(dir, "/tmp/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("changed=false on insert")
	}
	out, _ := os.ReadFile(repo)
	if !strings.HasPrefix(string(out), "# yaml-language-server: $schema=/tmp/schema.json\n") {
		t.Errorf("modeline not prepended:\n%s", out)
	}
}

func TestSetModelineReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, ".treeman.yaml")
	body := "# yaml-language-server: $schema=old\nrepo:\n  name: foo\n"
	if err := os.WriteFile(repo, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := SetModeline(dir, "new")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("changed=false on replace")
	}
	out, _ := os.ReadFile(repo)
	if !strings.Contains(string(out), "$schema=new") {
		t.Errorf("modeline not replaced:\n%s", out)
	}
	if strings.Contains(string(out), "$schema=old") {
		t.Errorf("old modeline left behind:\n%s", out)
	}
}

func TestSetModelineNoChangeWhenIdentical(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, ".treeman.yaml")
	body := "# yaml-language-server: $schema=same\nrepo:\n  name: foo\n"
	if err := os.WriteFile(repo, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := SetModeline(dir, "same")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("changed=true when modeline already matches")
	}
}

func TestSetModelineMissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	changed, err := SetModeline(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("changed=true with no .treeman.yaml")
	}
}

func TestReadModeline(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, ".treeman.yaml")
	_ = os.WriteFile(repo, []byte("# yaml-language-server: $schema=https://example.com/s.json\n"), 0o644)
	got := ReadModeline(dir)
	if got != "https://example.com/s.json" {
		t.Errorf("ReadModeline = %q, want https://example.com/s.json", got)
	}
}

func TestInstallTargetURLSkipsFileWrite(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".treeman.yaml"), []byte("repo: {}\n"), 0o644)
	resolved, _, err := Install(dir, TargetURL)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != URL {
		t.Errorf("resolved = %q, want %q", resolved, URL)
	}
	// File-write path should be empty for URL target.
	if _, err := os.Stat(filepath.Join(dir, "schemas")); err == nil {
		t.Errorf("schemas dir created for TargetURL")
	}
}

func TestInstallTargetRepoWritesFile(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".treeman.yaml"), []byte("repo: {}\n"), 0o644)
	resolved, _, err := Install(dir, TargetRepo)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "schemas", "treeman.schema.json")
	if resolved != want {
		t.Errorf("resolved = %q, want %q", resolved, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("schema file not written: %v", err)
	}
}
