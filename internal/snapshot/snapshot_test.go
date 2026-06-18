package snapshot

import (
	"strings"
	"testing"
)

func TestFingerprintChangesOnInputChange(t *testing.T) {
	k := New("mysql", "8.0.30", "filename", "abc123", "", nil)
	f1 := k.Fingerprint()
	k.MigrationsHashHex = "def456"
	f2 := k.Fingerprint()
	if f1 == f2 {
		t.Error("fingerprint should differ after changing migrations hash")
	}
}

// TestFingerprintIndependentOfDBName pins the v5 invariant: the cache
// key is content-only. Two prepares that differ ONLY in the database
// name they'll restore into (the per-worktree slug) must produce the
// same fingerprint so every worktree of one repo/branch shares a single
// template instead of cold-building its own. The DB name isn't an input
// to New at all — this test guards against it sneaking back in via a
// LockfileHashes entry or similar.
func TestFingerprintIndependentOfDBName(t *testing.T) {
	base := New("mysql", "8.0.30", "", "", "dumphash", map[string]string{
		"migrations":    "abc123",
		"composer.lock": "x",
	})
	// A second key built from identical content inputs must match,
	// regardless of which slugged DB it will be restored into.
	same := New("mysql", "8.0.30", "", "", "dumphash", map[string]string{
		"migrations":    "abc123",
		"composer.lock": "x",
	})
	if base.Fingerprint() != same.Fingerprint() {
		t.Fatalf("fingerprint must depend only on content inputs: %s vs %s",
			base.Fingerprint(), same.Fingerprint())
	}
	// Sanity: a real content change (dump hash) still flips it.
	changed := New("mysql", "8.0.30", "", "", "otherdump", map[string]string{
		"migrations":    "abc123",
		"composer.lock": "x",
	})
	if base.Fingerprint() == changed.Fingerprint() {
		t.Error("fingerprint should differ after changing the dump hash")
	}
}

func TestTemplateNameShape(t *testing.T) {
	// `_tm_<fingerprint[0:16]>` — engine name is intentionally NOT
	// in the DB name (snapshots.engine carries it), and `_tmpl_` is
	// redundant with the `_tm_` namespace marker.
	k := New("postgres", "16", "filename", "h", "", nil)
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
	k1 := New("mysql", "8.0", "filename", "abc", "dump1", map[string]string{"composer.lock": "x"})
	k2 := New("mysql", "8.0", "filename", "abc", "dump1", map[string]string{"composer.lock": "x"})
	if k1.TemplateName() != k2.TemplateName() {
		t.Errorf("expected deterministic names: %s vs %s", k1.TemplateName(), k2.TemplateName())
	}
}
