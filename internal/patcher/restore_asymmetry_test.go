package patcher

import (
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

// Probes for extract/restore asymmetry: a key that extractContent
// reports as present in HEAD but restore*FromHead's regex can't splice
// would leave the smudged (working) value in clean's output → the file
// shows as permanently modified. Each case asserts clean(working)==HEAD.

func TestProbeINIIndentedKey(t *testing.T) {
	auditClean(t, "ini_indented_key",
		config.Patch{File: "c.ini", Format: "ini", Set: map[string]string{"s.k": "{slug}"}},
		"[s]\n  k = head_val\n", "[s]\n  k = working_val\n")
}

func TestProbeINISpacedQuotedValue(t *testing.T) {
	auditClean(t, "ini_quoted_spaces",
		config.Patch{File: "c.ini", Format: "ini", Set: map[string]string{"s.k": "{slug}"}},
		"[s]\nk = \"head val\"\n", "[s]\nk = working_val\n")
}

func TestProbeTOMLSpacedQuotedValue(t *testing.T) {
	auditClean(t, "toml_quoted_spaces",
		config.Patch{File: "c.toml", Format: "toml", Set: map[string]string{"k": "{slug}"}},
		"k = \"head val\"\n", "k = working_val\n")
}

func TestProbeDotenvIndented(t *testing.T) {
	auditClean(t, "dotenv_export_prefix",
		config.Patch{File: ".env", Format: "dotenv", Set: map[string]string{"K": "{slug}"}},
		"K=head_val\n", "K=working_val\n")
}
