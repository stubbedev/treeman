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

// TestCleanStripsKeysAbsentFromHEAD_Dotenv is the regression test
// for the bug that caused kontainer `.env.testing` to show as
// modified on every fresh worktree: `patches[].set` listed
// `DB_DATABASE`, `REDIS_CACHE_DATABASE`, `REDIS_QUEUE_DATABASE` —
// none of which existed in HEAD's `.env.testing`. Apply appended
// them on smudge, but Clean only restored keys that HEAD already
// had. The appended lines stayed in the index after
// `git add --renormalize`, leaving the file permanently dirty.
//
// Fix contract: any `set:` key absent from HEAD must be STRIPPED
// from clean's output, so clean(working) == HEAD verbatim.
func TestCleanStripsKeysAbsentFromHEAD_Dotenv(t *testing.T) {
	canonical := "FOO=bar\n"
	working := "FOO=bar\nDB_DATABASE=app_feat-x\nREDIS_QUEUE_DATABASE=13\n"
	p := config.Patch{File: ".env", Set: map[string]string{
		"DB_DATABASE":          "app_{slug}",
		"REDIS_QUEUE_DATABASE": "{slug_redis_queue}",
	}}

	clean, err := Clean(p, working, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if clean != canonical {
		t.Fatalf("clean(working) != canonical\nwant: %q\ngot:  %q", canonical, clean)
	}
}

func TestCleanStripsKeysAbsentFromHEAD_Phpunit(t *testing.T) {
	canonical := `<?xml version="1.0"?>
<phpunit>
	<php>
		<env name="FOO" value="kept" force="true"/>
	</php>
</phpunit>
`
	working := `<?xml version="1.0"?>
<phpunit>
	<php>
		<env name="FOO" value="kept" force="true"/>
		<env name="DB_DATABASE" value="app_feat-x" force="true"/>
	</php>
</phpunit>
`
	p := config.Patch{File: "phpunit.xml", Format: "phpunit", Set: map[string]string{
		"DB_DATABASE": "app_{slug}",
	}}
	clean, err := Clean(p, working, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if clean != canonical {
		t.Fatalf("clean(working) != canonical\nwant: %q\ngot:  %q", canonical, clean)
	}
}

func TestCleanStripsKeysAbsentFromHEAD_Mixed(t *testing.T) {
	// Half the patched keys exist in HEAD (restore to HEAD), half don't
	// (strip). End state must equal HEAD verbatim — modulo the
	// non-patched user line that's also present in HEAD.
	canonical := "EXISTING=v1\nKEPT=user\n"
	working := "EXISTING=v1_feat-x\nKEPT=user\nAPPENDED=feat-x\n"
	p := config.Patch{File: ".env", Set: map[string]string{
		"EXISTING": "v1_{slug}",
		"APPENDED": "{slug}",
	}}
	clean, err := Clean(p, working, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if clean != canonical {
		t.Fatalf("clean(working) != canonical\nwant: %q\ngot:  %q", canonical, clean)
	}
}

func TestCleanStripsKeysAbsentFromHEAD_YAML(t *testing.T) {
	canonical := "kept: v1\n"
	working := "kept: v1\nappended: feat-x\n"
	p := config.Patch{File: "config.yml", Set: map[string]string{
		"appended": "{slug}",
	}}
	clean, err := Clean(p, working, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if strings.TrimSpace(clean) != strings.TrimSpace(canonical) {
		t.Fatalf("clean(working) != canonical\nwant: %q\ngot:  %q", canonical, clean)
	}
}

func TestCleanStripsKeysAbsentFromHEAD_JSON(t *testing.T) {
	canonical := `{"kept":"v1"}`
	working := `{"kept":"v1","appended":"feat-x"}`
	p := config.Patch{File: "config.json", Set: map[string]string{
		"appended": "{slug}",
	}}
	clean, err := Clean(p, working, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if !strings.Contains(clean, `"kept": "v1"`) {
		t.Errorf("kept key missing: %q", clean)
	}
	if strings.Contains(clean, "appended") {
		t.Errorf("appended key not stripped: %q", clean)
	}
}

func TestCleanStripsKeysAbsentFromHEAD_INI(t *testing.T) {
	canonical := "[database]\nkept = v1\n"
	working := "[database]\nkept = v1\nappended = feat-x\n"
	p := config.Patch{File: "config.ini", Set: map[string]string{
		"database.appended": "{slug}",
	}}
	clean, err := Clean(p, working, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if !strings.Contains(clean, "kept") {
		t.Errorf("kept key missing: %q", clean)
	}
	if strings.Contains(clean, "appended") {
		t.Errorf("appended key not stripped: %q", clean)
	}
}

// TestCleanRestoresPhpunitElementBytesVerbatim is the regression
// test for kontainer phpunit.xml showing as modified on every fresh
// worktree even after the absent-from-HEAD strip landed. HEAD carried
// `<env name="DB_DATABASE" value="kontainer_testing"/>` WITHOUT
// `force="true"`; Apply's smudge always emits the element WITH
// `force="true"`. Old Clean ran applyPhpunitContent on the working
// tree using HEAD's scalar, but applyPhpunit always re-renders the
// element with `force="true"` glued on — so clean output had
// `force="true"` and `git diff --cached` showed the attribute being
// added forever.
//
// Fix contract: for phpunit, clean must splice HEAD's exact element
// bytes back in — whatever attribute set HEAD has, preserved
// verbatim, so clean(working) == HEAD byte-for-byte.
func TestCleanRestoresPhpunitElementBytesVerbatim(t *testing.T) {
	canonical := `<?xml version="1.0"?>
<phpunit>
	<php>
		<env name="DB_DATABASE" value="kontainer_testing"/>
		<env name="MONGO_DB_DATABASE" value="mongodb_testing"/>
	</php>
</phpunit>
`
	working := `<?xml version="1.0"?>
<phpunit>
	<php>
		<env name="DB_DATABASE" value="kontainer_testing_feat-x" force="true"/>
		<env name="MONGO_DB_DATABASE" value="mongodb_testing_feat-x" force="true"/>
	</php>
</phpunit>
`
	p := config.Patch{File: "phpunit.xml", Format: "phpunit", Set: map[string]string{
		"DB_DATABASE":       "kontainer_testing_{slug}",
		"MONGO_DB_DATABASE": "mongodb_testing_{slug}",
	}}
	clean, err := Clean(p, working, canonical)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if clean != canonical {
		t.Fatalf("clean(working) != canonical (force=\"true\" leaked)\nwant: %q\ngot:  %q", canonical, clean)
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
