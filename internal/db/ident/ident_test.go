package ident

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty", "", false},
		{"alpha", "myapp", true},
		{"mixed", "myapp_test_proj_123", true},
		{"underscores", "__db__", true},
		{"upper", "MyApp_DB", true},
		{"hyphen", "my-app", false},
		{"dot", "my.app", false},
		{"backtick", "my`app", false},
		{"quote", `my"app`, false},
		{"space", "my app", false},
		{"semicolon", "x;DROP", false},
		{"unicode", "café", false},
		{"null", "x\x00y", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validate(c.in)
			if (err == nil) != c.ok {
				t.Fatalf("validate(%q): err=%v want ok=%v", c.in, err, c.ok)
			}
		})
	}
}

func TestQuoteMySQL(t *testing.T) {
	got, err := QuoteMySQL("myapp")
	if err != nil || got != "`myapp`" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if _, err := QuoteMySQL("bad-name"); err == nil {
		t.Fatal("expected error for bad-name")
	}
}

func TestQuotePostgres(t *testing.T) {
	got, err := QuotePostgres("myapp")
	if err != nil || got != `"myapp"` {
		t.Fatalf("got %q, err %v", got, err)
	}
	if _, err := QuotePostgres("bad name"); err == nil {
		t.Fatal("expected error for bad name")
	}
}
