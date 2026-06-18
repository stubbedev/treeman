package prepare

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestColdBuildEmitsDropEventPerEngine source-scans prepare.go and
// asserts every cold-build label block contains an
// `emitColdBuildDrop(` call. The production incident hid because the
// pre-drop step was silent — no `db_drop` event meant the wipe
// didn't show up in `logs_query`. A future edit that removes the
// emit call from any engine's path must fail this test instead of
// silently regressing observability.
//
// Light-touch source scan rather than a full integration test:
// running real cold-build per engine would need every driver up,
// which is what the e2e suite is for. The invariant we're guarding
// is purely "the event call site exists per engine".
func TestColdBuildEmitsDropEventPerEngine(t *testing.T) {
	src, err := os.ReadFile("prepare.go")
	if err != nil {
		t.Fatalf("read prepare.go: %v", err)
	}
	body := string(src)

	labels := []string{
		"mysqlColdBuild:",
		"pgColdBuild:",
		"mongoColdBuild:",
		"redisColdBuild:",
		"esColdBuild:",
	}
	for i, lbl := range labels {
		t.Run(strings.TrimSuffix(lbl, ":"), func(t *testing.T) {
			block := coldBuildBlock(t, body, lbl, labels, i)
			if !strings.Contains(block, "emitColdBuildDrop(") {
				t.Errorf(
					"cold-build label %q missing emitColdBuildDrop(...) call — silent pre-drop regresses logs_query observability",
					lbl,
				)
			}
		})
	}
}

// coldBuildBlock returns the source span for one cold-build label,
// bounded by either the next cold-build label OR the next
// "\nfunc " line — whichever comes first. Without the func bound
// the last label (esColdBuild) would extend through end-of-file and
// pick up unrelated DropMatching calls in TeardownDatabases.
func coldBuildBlock(t *testing.T, body, lbl string, labels []string, i int) string {
	t.Helper()
	start := strings.Index(body, lbl)
	if start < 0 {
		t.Fatalf("label %q not found in prepare.go — test or refactor is stale", lbl)
	}
	end := len(body)
	if pos := strings.Index(body[start+len(lbl):], "\nfunc "); pos >= 0 {
		end = start + len(lbl) + pos
	}
	for j, other := range labels {
		if j == i {
			continue
		}
		if pos := strings.Index(body[start+len(lbl):], other); pos >= 0 {
			if cand := start + len(lbl) + pos; cand < end {
				end = cand
			}
		}
	}
	return body[start:end]
}

// TestColdBuildUsesSlugAnchoredDrop ensures the cold-build pre-drop
// uses the exact-name (DropDatabase) or filtered (DropMatchingFiltered /
// DropPrefixFiltered) primitives, NOT the unbounded prefix-LIKE
// DropMatching / DropPrefix. The original sibling-wipe bug came from
// the unbounded variants; banning them in cold-build is the
// structural guard.
func TestColdBuildUsesSlugAnchoredDrop(t *testing.T) {
	src, err := os.ReadFile("prepare.go")
	if err != nil {
		t.Fatalf("read prepare.go: %v", err)
	}
	body := string(src)

	// Each cold-build block must NOT contain a bare DropMatching /
	// DropPrefix call. The filtered variants are fine.
	// Word-boundary on the close paren so `DropMatchingFiltered` /
	// `DropPrefixFiltered` (which take a third predicate arg) don't
	// false-positive on the bare 2-arg form.
	banned := regexp.MustCompile(`drv\.(DropMatching|DropPrefix)\(ctx, [^,]+\)`)
	labels := []string{"mysqlColdBuild:", "pgColdBuild:", "mongoColdBuild:", "redisColdBuild:", "esColdBuild:"}
	for i, lbl := range labels {
		t.Run(strings.TrimSuffix(lbl, ":"), func(t *testing.T) {
			block := coldBuildBlock(t, body, lbl, labels, i)
			if banned.MatchString(block) {
				t.Errorf(
					"cold-build label %q calls bare DropMatching/DropPrefix — must use exact DropDatabase or DropMatchingFiltered/DropPrefixFiltered to avoid sibling-worktree wipe",
					lbl,
				)
			}
		})
	}
}
