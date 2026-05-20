package envfile

import "testing"

func TestBasicKV(t *testing.T) {
	e := Parse("FOO=bar\nBAZ=qux\n")
	if v, _ := e.Get("FOO"); v != "bar" {
		t.Errorf("FOO=%q want bar", v)
	}
	if v, _ := e.Get("BAZ"); v != "qux" {
		t.Errorf("BAZ=%q want qux", v)
	}
}

func TestQuotedValues(t *testing.T) {
	e := Parse(`A="hello world"
B='single quoted'
`)
	if v, _ := e.Get("A"); v != "hello world" {
		t.Errorf("A=%q", v)
	}
	if v, _ := e.Get("B"); v != "single quoted" {
		t.Errorf("B=%q", v)
	}
}

func TestExportPrefix(t *testing.T) {
	e := Parse("export FOO=bar\n")
	if v, _ := e.Get("FOO"); v != "bar" {
		t.Errorf("export FOO=%q", v)
	}
}

func TestInlineCommentUnquotedOnly(t *testing.T) {
	e := Parse(`FOO=bar # trailing
BAZ="x # not a comment"
`)
	if v, _ := e.Get("FOO"); v != "bar" {
		t.Errorf("FOO=%q (trailing comment should be stripped)", v)
	}
	if v, _ := e.Get("BAZ"); v != "x # not a comment" {
		t.Errorf("BAZ=%q (quoted # should be preserved)", v)
	}
}

func TestCommentsSkipped(t *testing.T) {
	e := Parse("# top\nFOO=bar\n#mid\nBAZ=qux\n")
	if len(e.Vars) != 2 {
		t.Errorf("want 2 vars, got %d", len(e.Vars))
	}
}

func TestExpandLocal(t *testing.T) {
	e := Parse("FOO=hello\nBAR=${FOO}/world\n")
	if v, _ := e.Get("BAR"); v != "hello/world" {
		t.Errorf("BAR=%q want hello/world", v)
	}
}

func TestIgnoresInvalidKeys(t *testing.T) {
	e := Parse("12 = no\nFOO=ok\n  =empty\n")
	if len(e.Vars) != 1 {
		t.Errorf("want 1 var, got %d (%v)", len(e.Vars), e.Vars)
	}
	if v, _ := e.Get("FOO"); v != "ok" {
		t.Errorf("FOO=%q", v)
	}
}
