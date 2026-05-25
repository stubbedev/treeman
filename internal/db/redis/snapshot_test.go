package redis

import (
	"context"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// newTestDriver builds a Driver whose pool will never dial — the
// guards under test fire before any network call.
func newTestDriver(t *testing.T) *Driver {
	t.Helper()
	opts, err := redis.ParseURL("redis://localhost:6379/0")
	if err != nil {
		t.Fatal(err)
	}
	return &Driver{baseOpts: opts, clients: map[int]*redis.Client{}}
}

// TestDropPrefixRejectsEmpty asserts the safety guard that prevents
// `DropPrefix("")` from SCANning the entire keyspace and DELing
// everything. This is the most dangerous footgun in the prefix
// model — a typo in `prefix_template` could otherwise wipe Redis.
func TestDropPrefixRejectsEmpty(t *testing.T) {
	d := newTestDriver(t) // not dialed; guard fires before any network call.
	_, err := d.DropPrefix(context.Background(), "")
	if err == nil {
		t.Fatal("DropPrefix(\"\") should error, not wipe the keyspace")
	}
	if !strings.Contains(err.Error(), "empty prefix") {
		t.Errorf("error should mention empty prefix, got: %v", err)
	}
}

// TestSnapshotCreateRejectsSameSrcDst asserts the symmetric guard:
// copying a prefix onto itself would either be a no-op (under
// REPLACE) or destroy the source. Either way the call is a bug.
func TestSnapshotCreateRejectsSameSrcDst(t *testing.T) {
	d := newTestDriver(t)
	err := d.SnapshotCreate(context.Background(), "foo:", "foo:")
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("expected differ-error, got %v", err)
	}
	err = d.SnapshotRestore(context.Background(), "foo:", "foo:")
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("expected differ-error, got %v", err)
	}
}

// TestVersionAtLeast covers the parser used for the COPY-vs-
// DUMP+RESTORE strategy pick. Edge cases the parser must handle:
// trailing pre-release suffix, missing patch, non-numeric noise.
func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		v          string
		major, min int
		want       bool
	}{
		{"6.2.0", 6, 2, true},
		{"6.2.7", 6, 2, true},
		{"7.0.0", 6, 2, true},
		{"6.1.99", 6, 2, false},
		{"5.0.7", 6, 2, false},
		{"7.0.5-rc1", 6, 2, true},
		{"6.2", 6, 2, true},
		{"6.2.0-alpha", 6, 2, true},
		{"abc", 6, 2, false},
		{"", 6, 2, false},
		{"6", 6, 2, false}, // missing minor
	}
	for _, tc := range cases {
		got := versionAtLeast(tc.v, tc.major, tc.min)
		if got != tc.want {
			t.Errorf("versionAtLeast(%q, %d, %d) = %v, want %v", tc.v, tc.major, tc.min, got, tc.want)
		}
	}
}
