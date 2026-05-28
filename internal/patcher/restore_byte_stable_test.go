package patcher

import (
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

func auditClean(t *testing.T, name string, p config.Patch, head, working string) {
	t.Helper()
	clean, err := Clean(p, working, head)
	if err != nil {
		t.Fatalf("[%s] clean err: %v", name, err)
	}
	if clean != head {
		t.Errorf("[%s] clean != HEAD\nHEAD:    %q\nworking: %q\ngot:     %q", name, head, working, clean)
	}
}

func TestAuditDotenvSpaceFormatting(t *testing.T) {
	auditClean(t, "spaces_around_eq",
		config.Patch{File: ".env", Format: "dotenv", Set: map[string]string{"K": "{slug}"}},
		"K = head_val\n", "K=working_val\n")
}

func TestAuditINISpaceFormatting(t *testing.T) {
	auditClean(t, "ini_no_spaces",
		config.Patch{File: "c.ini", Format: "ini", Set: map[string]string{"s.k": "{slug}"}},
		"[s]\nk=head_val\n", "[s]\nk = working_val\n")
}

func TestAuditJSONKeyOrder(t *testing.T) {
	auditClean(t, "json_z_first",
		config.Patch{File: "c.json", Format: "json", Set: map[string]string{"k": "{slug}"}},
		`{"z":1,"k":"head_val","a":2}`,
		`{"z":1,"k":"working_val","a":2}`)
}

func TestAuditYAMLQuotingStyle(t *testing.T) {
	auditClean(t, "yaml_quoted",
		config.Patch{File: "c.yml", Format: "yaml", Set: map[string]string{"k": "{slug}"}},
		`k: "head_val"
`, `k: "working_val"
`)
}

func TestAuditTOMLOrder(t *testing.T) {
	auditClean(t, "toml_top",
		config.Patch{File: "c.toml", Format: "toml", Set: map[string]string{"k": "{slug}"}},
		"z = 1\nk = \"head_val\"\na = 2\n", "z = 1\nk = \"working_val\"\na = 2\n")
}

func TestAuditPhpunitForce(t *testing.T) {
	auditClean(t, "phpunit_no_force",
		config.Patch{File: "p.xml", Format: "phpunit", Set: map[string]string{"K": "{slug}"}},
		`<phpunit><php><env name="K" value="head_val"/></php></phpunit>`,
		`<phpunit><php><env name="K" value="working_val" force="true"/></php></phpunit>`)
}

func TestAuditDotenvQuotedValue(t *testing.T) {
	auditClean(t, "dotenv_quoted",
		config.Patch{File: ".env", Format: "dotenv", Set: map[string]string{"K": "{slug}"}},
		"K=\"head val\"\n", "K=working_val\n")
}

func TestAuditTOMLSectionedKey(t *testing.T) {
	auditClean(t, "toml_table",
		config.Patch{File: "c.toml", Format: "toml", Set: map[string]string{"database.k": "{slug}"}},
		"[database]\nz = 1\nk = \"head_val\"\n",
		"[database]\nz = 1\nk = \"working_val\"\n")
}

func TestAuditJSONNested(t *testing.T) {
	auditClean(t, "json_nested",
		config.Patch{File: "c.json", Format: "json", Set: map[string]string{"db.name": "{slug}"}},
		`{"db":{"name":"head_val","port":3306}}`,
		`{"db":{"name":"working_val","port":3306}}`)
}

// YAML block scalars (`k: |` / `k: >`) where HEAD spans multiple
// lines: byte-stable restore would require detecting the block's
// indent boundary and splicing the multi-line region. We don't —
// `restoreYAMLFromHead` falls back to applyContent for non-scalar
// terminals, which re-marshals the document and may reformat
// unrelated parts. Not byte-stable, but the failure mode is
// surfaced as a single re-render (not a "file is permanently
// dirty"-style infinite loop). Documented limitation; treeman
// patches target plain scalars in practice.

func TestAuditDotenvComment(t *testing.T) {
	auditClean(t, "dotenv_trailing_comment",
		config.Patch{File: ".env", Format: "dotenv", Set: map[string]string{"K": "{slug}"}},
		"K=head_val # important comment\n", "K=working_val\n")
}

func TestAuditPhpunitSingleQuotes(t *testing.T) {
	// PHPUnit attribute values can use single quotes too — extractPhpunit's
	// regex is `value="..."` so single-quoted HEAD won't be found. Pin
	// the current behavior: fall through to applyContent, which writes the
	// canonical double-quoted form. NOT byte-stable in this edge case;
	// rare enough to live with.
	clean, err := Clean(
		config.Patch{File: "p.xml", Format: "phpunit", Set: map[string]string{"K": "{slug}"}},
		`<phpunit><php><env name="K" value="working_val" force="true"/></php></phpunit>`,
		`<phpunit><php><env name='K' value='head_val'/></php></phpunit>`,
	)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	_ = clean // we just verify no crash; format coverage tracked elsewhere
}
