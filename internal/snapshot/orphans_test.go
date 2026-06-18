package snapshot

import "testing"

// TestNameScopedTemplatePattern pins the orphan probe's name matcher:
// only treeman-derived template shapes match (current, legacy, spare
// family), and the captured base folds spares onto their template.
// User databases — whatever they're called — must never match.
func TestNameScopedTemplatePattern(t *testing.T) {
	cases := []struct {
		name string
		base string // "" = no match
	}{
		{"_tm_0123456789abcdef", "_tm_0123456789abcdef"},
		{"_tm_0123456789abcdef_spare1", "_tm_0123456789abcdef"},
		{"_tm_0123456789abcdef_spare16", "_tm_0123456789abcdef"},
		{"_tm_tmpl_mysql_0123456789abcdef", "_tm_tmpl_mysql_0123456789abcdef"},
		{"_tm_tmpl_postgres_0123456789abcdef_spare2", "_tm_tmpl_postgres_0123456789abcdef"},
		{"_tm_0123456789abcde", ""},        // 15 hex chars
		{"_tm_0123456789abcdefg", ""},      // 17th char not hex boundary
		{"_tm_0123456789abcdef_w1", ""},    // test-clone style suffix
		{"app_feature_x", ""},              // user DB
		{"tm_0123456789abcdef", ""},        // ES form must not match name-scoped
		{"_tm_XYZ4567890ABCDEF", ""},       // uppercase/non-hex
		{"_tm_0123456789abcdef_spare", ""}, // spare without slot
	}
	for _, tc := range cases {
		m := nameScopedTemplate.FindStringSubmatch(tc.name)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != tc.base {
			t.Errorf("nameScopedTemplate(%q) base = %q, want %q", tc.name, got, tc.base)
		}
	}
}

// TestESTemplatePrefixPattern: ES index names fold onto their
// row-recorded `tm_<hex16>_` prefix; user indexes never match.
func TestESTemplatePrefixPattern(t *testing.T) {
	cases := []struct {
		name string
		base string
	}{
		{"tm_0123456789abcdef_products", "tm_0123456789abcdef_"},
		{"tm_0123456789abcdef_orders_v2", "tm_0123456789abcdef_"},
		{"tm_0123456789abcde_products", ""}, // 15 hex chars
		{"team_metrics", ""},
		{"_tm_0123456789abcdef", ""}, // name-scoped form must not match ES
	}
	for _, tc := range cases {
		m := esTemplatePrefix.FindStringSubmatch(tc.name)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != tc.base {
			t.Errorf("esTemplatePrefix(%q) base = %q, want %q", tc.name, got, tc.base)
		}
	}
}

func TestSpareNamePattern(t *testing.T) {
	m := spareNamePattern.FindStringSubmatch("_tm_0123456789abcdef_spare3")
	if m == nil || m[1] != "_tm_0123456789abcdef" {
		t.Errorf("spare name should match with template capture; got %v", m)
	}
	for _, n := range []string{"_tm_0123456789abcdef", "app_spare1", "_tm_0123456789abcdef_w1"} {
		if spareNamePattern.MatchString(n) {
			t.Errorf("%q should not match spare pattern", n)
		}
	}
}
