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

func TestTemplateNameIncludesEngineAndPrefix(t *testing.T) {
	k := New("postgres", "16", "myapp_test", "rails", "filename", "h", "", nil)
	n := k.TemplateName()
	if !strings.HasPrefix(n, "_tm_tmpl_postgres_") {
		t.Errorf("template name: %s", n)
	}
	if len(n) != len("_tm_tmpl_postgres_")+16 {
		t.Errorf("template name length: %d", len(n))
	}
}
