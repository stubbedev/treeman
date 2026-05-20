package patcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tmpfile(t *testing.T, content string) string {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "f")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvReplaceExistingKey(t *testing.T) {
	p := tmpfile(t, "FOO=old\nBAR=keep\n")
	r, err := PatchEnvFile(p, []Pair{{"FOO", "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if r != Updated {
		t.Errorf("want Updated, got %v", r)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "FOO=new\nBAR=keep\n" {
		t.Errorf("got %q", string(b))
	}
}

func TestEnvAppendMissingKey(t *testing.T) {
	p := tmpfile(t, "FOO=keep\n")
	r, err := PatchEnvFile(p, []Pair{{"BAR", "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if r != Updated {
		t.Errorf("want Updated, got %v", r)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "FOO=keep\nBAR=new\n" {
		t.Errorf("got %q", string(b))
	}
}

func TestEnvIdempotentWhenValueMatches(t *testing.T) {
	p := tmpfile(t, "FOO=already\n")
	r, _ := PatchEnvFile(p, []Pair{{"FOO", "already"}})
	if r != Unchanged {
		t.Errorf("want Unchanged, got %v", r)
	}
}

func TestPhpunitReplaceExistingEnv(t *testing.T) {
	p := tmpfile(t, "<phpunit>\n<php>\n\t<env name=\"FOO\" value=\"old\"/>\n\t</php>\n</phpunit>\n")
	if _, err := PatchPhpunitFile(p, []Pair{{"FOO", "new"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `<env name="FOO" value="new" force="true"/>`) {
		t.Errorf("missing replaced env: %s", string(b))
	}
}

func TestPhpunitInjectNewEnv(t *testing.T) {
	p := tmpfile(t, "<phpunit>\n<php>\n\t</php>\n</phpunit>\n")
	if _, err := PatchPhpunitFile(p, []Pair{{"FOO", "bar"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `<env name="FOO" value="bar" force="true"/>`) {
		t.Errorf("missing injected env: %s", string(b))
	}
}
