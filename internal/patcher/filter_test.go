package patcher

import (
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/template"
)

// tplCtx returns a template.Context tied to a stable slug used by
// the filter tests. Tests assert the rendered value through the
// smudge path, so the slug must be predictable.
func tplCtx() template.Context {
	return template.FromSlug(slug.Slug{Value: "feat-x", Source: slug.SourceTicket})
}

// TestSmudgeClean<Driver>Roundtrip locks the canonical invariant of
// filter mode: clean(smudge(canonical)) == canonical. With this
// equality, `git status` sees the working tree as matching the index
// for patched keys, which is what allows `git pull` to overwrite the
// file instead of refusing with "would be overwritten by merge".
//
// Each driver gets its own table entry: smudge populates patched
// keys with the per-worktree value, clean projects them back to the
// canonical (HEAD) value, and the result must equal the input we
// started with byte-for-byte after the canonical's reformat (yaml
// and toml round-trip through their marshallers so the canonical
// post-clean isn't textually identical to the original — we compare
// to a separately-marshalled canonical for those drivers).
func TestSmudgeCleanRoundtripDotenv(t *testing.T) {
	canonical := "DB_DATABASE=app\nFOO=bar\n"
	p := config.Patch{File: ".env", Set: map[string]string{"DB_DATABASE": "app_{slug}"}}

	smudged, err := Smudge(p, canonical, tplCtx())
	if err != nil {
		t.Fatalf("smudge: %v", err)
	}
	if !strings.Contains(smudged, "DB_DATABASE=app_feat-x") {
		t.Fatalf("smudge did not write per-worktree value: %q", smudged)
	}
	if !strings.Contains(smudged, "FOO=bar") {
		t.Fatalf("smudge dropped non-patched key: %q", smudged)
	}

	clean, err := Clean(p, smudged, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if clean != canonical {
		t.Fatalf("clean(smudge(x)) != x\nwant: %q\ngot:  %q", canonical, clean)
	}
}

func TestSmudgeCleanRoundtripPhpunit(t *testing.T) {
	canonical := `<?xml version="1.0"?>
<phpunit>
	<php>
		<env name="DB_TEST_DATABASE" value="app_testing" force="true"/>
	</php>
</phpunit>
`
	p := config.Patch{File: "phpunit.xml", Format: "phpunit", Set: map[string]string{"DB_TEST_DATABASE": "app_testing_{slug}"}}

	smudged, err := Smudge(p, canonical, tplCtx())
	if err != nil {
		t.Fatalf("smudge: %v", err)
	}
	if !strings.Contains(smudged, `value="app_testing_feat-x"`) {
		t.Fatalf("smudge value not in output: %q", smudged)
	}

	clean, err := Clean(p, smudged, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if clean != canonical {
		t.Fatalf("clean(smudge(x)) != x\nwant: %q\ngot:  %q", canonical, clean)
	}
}

func TestSmudgeCleanRoundtripINI(t *testing.T) {
	canonical := "[database]\nname = app\nport = 3306\n"
	p := config.Patch{File: "config.ini", Set: map[string]string{"database.name": "app_{slug}"}}

	smudged, err := Smudge(p, canonical, tplCtx())
	if err != nil {
		t.Fatalf("smudge: %v", err)
	}
	if !strings.Contains(smudged, "name = app_feat-x") {
		t.Fatalf("smudge value not in output: %q", smudged)
	}

	clean, err := Clean(p, smudged, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if strings.TrimSpace(clean) != strings.TrimSpace(canonical) {
		t.Fatalf("clean(smudge(x)) != x\nwant: %q\ngot:  %q", canonical, clean)
	}
}

// TestCleanLeavesNonPatchedKeys verifies that user edits to keys
// NOT in patches.set still surface as a real diff against HEAD —
// otherwise the filter would silently hide unrelated edits to the
// same file.
func TestCleanLeavesNonPatchedKeys(t *testing.T) {
	canonical := "DB_DATABASE=app\nFOO=bar\n"
	working := "DB_DATABASE=app_feat-x\nFOO=user-edited\n"
	p := config.Patch{File: ".env", Set: map[string]string{"DB_DATABASE": "app_{slug}"}}

	clean, err := Clean(p, working, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if !strings.Contains(clean, "DB_DATABASE=app\n") {
		t.Errorf("patched key not restored to canonical: %q", clean)
	}
	if !strings.Contains(clean, "FOO=user-edited") {
		t.Errorf("non-patched edit was stripped: %q", clean)
	}
}

// TestCleanWithoutHEADContentLeavesWorkingTree covers the
// brand-new-file case: nothing committed upstream yet means no
// canonical to project onto. Clean must pass the working tree
// through verbatim — committing it then becomes the first canonical
// (and subsequent pulls work normally).
func TestCleanWithoutHEADContentLeavesWorkingTree(t *testing.T) {
	working := "DB_DATABASE=app_feat-x\nFOO=bar\n"
	p := config.Patch{File: ".env", Set: map[string]string{"DB_DATABASE": "app_{slug}"}}

	clean, err := Clean(p, working, "")
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if clean != working {
		t.Fatalf("clean with empty HEAD must be a no-op\nwant: %q\ngot:  %q", working, clean)
	}
}
