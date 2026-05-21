package snapshot

import (
	"strings"
	"testing"
)

func TestFingerprintChangesOnInputChange(t *testing.T) {
	k := New("mysql", "8.0.30", "myapp_testing_proj_1234", "laravel", "filename", "abc123", "", nil)
	f1 := k.Fingerprint()
	k.MigrationsHashHex = "def456"
	f2 := k.Fingerprint()
	if f1 == f2 {
		t.Error("fingerprint should differ after changing migrations hash")
	}
}

func TestTemplateNameShape(t *testing.T) {
	// `_tm_<fingerprint[0:16]>` — engine name is intentionally NOT
	// in the DB name (snapshots.engine carries it), and `_tmpl_` is
	// redundant with the `_tm_` namespace marker.
	k := New("postgres", "16", "myapp_test", "rails", "filename", "h", "", nil)
	n := k.TemplateName()
	if !strings.HasPrefix(n, "_tm_") {
		t.Errorf("template name missing namespace prefix: %s", n)
	}
	if strings.Contains(n, "postgres") || strings.Contains(n, "tmpl") {
		t.Errorf("template name leaks engine / legacy token: %s", n)
	}
	if len(n) != len("_tm_")+16 {
		t.Errorf("template name length: %d (want %d)", len(n), len("_tm_")+16)
	}
}

func TestTemplateNameDeterministic(t *testing.T) {
	// Same fingerprint inputs → same name, so a cold rebuild and a
	// cache-hit lookup land on the same DB identifier even if the
	// SQLite row got dropped between runs.
	k1 := New("mysql", "8.0", "src", "laravel", "filename", "abc", "dump1", map[string]string{"composer.lock": "x"})
	k2 := New("mysql", "8.0", "src", "laravel", "filename", "abc", "dump1", map[string]string{"composer.lock": "x"})
	if k1.TemplateName() != k2.TemplateName() {
		t.Errorf("expected deterministic names: %s vs %s", k1.TemplateName(), k2.TemplateName())
	}
}
