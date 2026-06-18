package runid

import (
	"context"
	"testing"
)

func TestNew_IsEightHexChars(t *testing.T) {
	id := New()
	if len(id) != 8 {
		t.Fatalf("want 8 chars, got %d (%q)", len(id), id)
	}
	for _, c := range id {
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !ok {
			t.Fatalf("non-hex char %q in %q", c, id)
		}
	}
}

func TestNew_DistinctAcrossCalls(t *testing.T) {
	// 32 bits of entropy collides ~1 in 4 billion per call pair.
	// Asserting 100 fresh ids are all unique is a reliable smoke test
	// that crypto/rand is wired and didn't fall back to a constant.
	seen := map[string]struct{}{}
	for range 100 {
		id := New()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q in 100 generations", id)
		}
		seen[id] = struct{}{}
	}
}

func TestFrom_NoIDReturnsEmpty(t *testing.T) {
	if got := From(context.Background()); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestFrom_NilCtxIsSafe(t *testing.T) {
	var ctx context.Context // deliberately nil — From must not panic
	if got := From(ctx); got != "" {
		t.Fatalf("want empty for nil ctx, got %q", got)
	}
}

func TestWithFromRoundTrip(t *testing.T) {
	ctx := With(context.Background(), "abc12345")
	if got := From(ctx); got != "abc12345" {
		t.Fatalf("want abc12345, got %q", got)
	}
}

func TestWith_EmptyIsNoop(t *testing.T) {
	base := With(context.Background(), "first")
	// Passing "" must NOT overwrite the existing id — the helper is
	// designed so callers can blanket-wrap without thinking.
	ctx := With(base, "")
	if got := From(ctx); got != "first" {
		t.Fatalf("With(\"\") clobbered existing id; got %q", got)
	}
}

func TestWith_NestedOverridesOuter(t *testing.T) {
	outer := With(context.Background(), "outer")
	inner := With(outer, "inner")
	if got := From(inner); got != "inner" {
		t.Fatalf("inner ctx should win; got %q", got)
	}
	// Outer is unchanged — context.WithValue is functional.
	if got := From(outer); got != "outer" {
		t.Fatalf("outer ctx mutated; got %q", got)
	}
}
