package mcp

import (
	"net/http"
	"testing"
)

func TestRootPath(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"   ":                    "",
		"/abs/path":              "/abs/path",
		"file:///abs/path":       "/abs/path",
		"file:///abs/with space": "/abs/with space",
		"  /trimmed  ":           "/trimmed",
		"relative/path":          "", // non-absolute roots are rejected
		"file://relative":        "", // no leading slash after authority -> not absolute
	}
	for in, want := range cases {
		if got := rootPath(in); got != want {
			t.Errorf("rootPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRootHeaders(t *testing.T) {
	h := http.Header{}
	// Lower-precedence header set first; X-Repo-Root must still win order.
	h.Set("Mcp-Root", "/from/mcp-root")
	h.Set("X-Repo-Root", "file:///from/repo-root")
	got := parseRootHeaders(h)
	if len(got) == 0 || got[0] != "/from/repo-root" {
		t.Fatalf("X-Repo-Root should rank first, got %v", got)
	}

	// Comma-separated values split into multiple roots.
	h2 := http.Header{}
	h2.Set("X-Mcp-Roots", "/a, file:///b ,  ")
	got2 := parseRootHeaders(h2)
	if len(got2) != 2 || got2[0] != "/a" || got2[1] != "/b" {
		t.Fatalf("comma split failed: %v", got2)
	}

	if r := parseRootHeaders(nil); r != nil {
		t.Fatalf("nil header should yield nil, got %v", r)
	}
}

func TestReqResolverPrecedence(t *testing.T) {
	// Header roots win over roots/list, and listRoots must not be called.
	called := false
	r := &reqResolver{
		headerRoots: []string{"/header/root"},
		listRoots:   func() []string { called = true; return []string{"/list/root"} },
	}
	if got := r.rootDir(); got != "/header/root" {
		t.Fatalf("header root should win, got %q", got)
	}
	if called {
		t.Fatal("listRoots must not be called when header roots are present")
	}

	// No header: fall back to roots/list, and memoize — a single tool
	// call resolves both repo and worktree, so listRoots must fire once.
	calls := 0
	r2 := &reqResolver{listRoots: func() []string { calls++; return []string{"/list/root"} }}
	if got := r2.rootDir(); got != "/list/root" {
		t.Fatalf("roots/list fallback failed, got %q", got)
	}
	if got := r2.rootDir(); got != "/list/root" {
		t.Fatalf("second rootDir() = %q, want cached /list/root", got)
	}
	if calls != 1 {
		t.Fatalf("listRoots called %d times, want 1 (memoized)", calls)
	}

	// Nothing available: empty (resolveRepo then uses cwd).
	if got := (&reqResolver{}).rootDir(); got != "" {
		t.Fatalf("empty resolver should yield empty, got %q", got)
	}
	var nilR *reqResolver
	if got := nilR.rootDir(); got != "" {
		t.Fatalf("nil resolver should yield empty, got %q", got)
	}
}
