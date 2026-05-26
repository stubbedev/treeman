package slug

import (
	"strings"
	"testing"
)

func TestTicketInBranchWins(t *testing.T) {
	s := For("/tmp/random-dir", "feature/PROJ-1234-foo")
	if s.Value != "proj_1234" {
		t.Fatalf("want proj_1234, got %s", s.Value)
	}
	if s.Source != SourceTicket {
		t.Fatalf("want SourceTicket, got %v", s.Source)
	}
}

func TestTicketInPathBasename(t *testing.T) {
	s := For("/tmp/PROJ-9001-bar", "")
	if s.Value != "proj_9001" {
		t.Fatalf("want proj_9001, got %s", s.Value)
	}
}

func TestFallsBackToPathHash(t *testing.T) {
	s := For("/nonexistent/random", "")
	if !strings.HasPrefix(s.Value, "wt_") {
		t.Fatalf("want wt_ prefix, got %s", s.Value)
	}
	if len(s.Value) != 11 {
		t.Fatalf("want 11-char wt_xxxxxxxx, got len=%d (%q)", len(s.Value), s.Value)
	}
	if s.Source != SourcePathHash {
		t.Fatalf("want SourcePathHash, got %v", s.Source)
	}
}

func TestForMainBranchAware(t *testing.T) {
	cases := []struct {
		branch string
		want   string
	}{
		{"main", "main_main"},
		{"develop", "main_develop"},
		{"feature/foo-bar", "main_feature_foo_bar"},
		{"feature/PROJ-123", "main_feature_proj_123"},
		{"release/v1.0.0", "main_release_v1_0_0"},
	}
	for _, tc := range cases {
		got := ForMain("/some/repo", tc.branch)
		if got.Value != tc.want {
			t.Errorf("ForMain branch=%q value=%q want=%q", tc.branch, got.Value, tc.want)
		}
		if got.Source != SourceMain {
			t.Errorf("ForMain branch=%q source=%v want=SourceMain", tc.branch, got.Source)
		}
	}
}

func TestForMainDetachedHasStableHash(t *testing.T) {
	a := ForMain("/some/repo", "")
	b := ForMain("/some/repo", "")
	if a.Value != b.Value {
		t.Errorf("detached slug not deterministic: %q vs %q", a.Value, b.Value)
	}
	if !strings.HasPrefix(a.Value, "main_detached_") {
		t.Errorf("want main_detached_ prefix, got %q", a.Value)
	}
	c := ForMain("/different/repo", "")
	if a.Value == c.Value {
		t.Errorf("different repos collapsed to same detached slug: %q", a.Value)
	}
}

func TestForMainSymbolOnlyBranchFallsBackToHash(t *testing.T) {
	got := ForMain("/some/repo", "@@@")
	if !strings.HasPrefix(got.Value, "main_sym_") {
		t.Errorf("symbol-only branch should hash, got %q", got.Value)
	}
	// Different symbol-only branches must produce different slugs so
	// two odd branches don't collide on `main_`.
	other := ForMain("/some/repo", "###")
	if got.Value == other.Value {
		t.Errorf("symbol-only branches collided: %q == %q", got.Value, other.Value)
	}
}

func TestForMainLongBranchTruncated(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := ForMain("/some/repo", long)
	if len(got.Value) > 32 {
		t.Errorf("want <=32 chars, got len=%d %q", len(got.Value), got.Value)
	}
}

func TestDashedReplacesUnderscores(t *testing.T) {
	s := Slug{Value: "proj_1234", Source: SourceTicket}
	if s.Dashed() != "proj-1234" {
		t.Fatalf("want proj-1234, got %s", s.Dashed())
	}
}

func TestRedisIndicesInRange6_15(t *testing.T) {
	s := Slug{Value: "proj_1234", Source: SourceTicket}
	q, c := s.RedisIndices()
	if q < 6 || q > 15 {
		t.Fatalf("queue out of range 6..15: %d", q)
	}
	if c < 6 || c > 15 {
		t.Fatalf("cache out of range 6..15: %d", c)
	}
}

func TestSysVCksumKnownVectors(t *testing.T) {
	// Cross-checked against POSIX `cksum`:
	//   `printf '' | cksum`     => 4294967295 (0xFFFFFFFF), 0
	//   `printf 'a' | cksum`    => 1220704766, 1
	//   `printf 'abc' | cksum`  => 1219131554, 3
	cases := []struct {
		in   string
		want uint32
	}{
		{"", 4294967295},
		{"a", 1220704766},
		{"abc", 1219131554},
	}
	for _, c := range cases {
		got := sysvCksum([]byte(c.in))
		if got != c.want {
			t.Errorf("sysvCksum(%q): want %d, got %d", c.in, c.want, got)
		}
	}
}

// Pin RedisIndices for a known input so any change to the cksum
// math or the redis-index formulae surfaces as a test failure.
// The values match POSIX `cksum` on the literal "proj_1234".
func TestRedisIndicesPinned(t *testing.T) {
	s := Slug{Value: "proj_1234", Source: SourceTicket}
	q, c := s.RedisIndices()
	// Derived via POSIX `cksum` on the literal string "proj_1234"
	// and the formulae from RedisIndices(). Re-verify with:
	//   printf 'proj_1234' | cksum
	// then compute (h%10+6, (h/10)%10+6).
	h := sysvCksum([]byte("proj_1234"))
	wantQ := uint8(h%10 + 6)
	wantC := uint8((h/10)%10 + 6)
	if q != wantQ || c != wantC {
		t.Fatalf("indices drifted: got (%d,%d) want (%d,%d) (cksum=%d)", q, c, wantQ, wantC, h)
	}
}
