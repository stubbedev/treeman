package store

import (
	"context"
	"path/filepath"
	"testing"
)

// openRepoStore opens a fresh store with repo id 1 registered.
func openRepoStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.EnsureRepo(ctx, "/r", "r"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFindRollbackAncestor_EditMidSequence(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	// Ancestor: 001,002(OLD),003. Current: 002 edited in place.
	anc := record("fpEdit", "ch", map[string]InputVector{migs: {
		{Path: "001.sql", Hash: "h1"},
		{Path: "002.sql", Hash: "hOLD"},
		{Path: "003.sql", Hash: "h3"},
	}})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"},
			{Path: "002.sql", Hash: "hNEW"},
			{Path: "003.sql", Hash: "h3"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Fingerprint != "fpEdit" {
		t.Fatalf("expected fpEdit ancestor, got %v", got)
	}
	// Common prefix = 1 (001), unwind 002 + 003 = 2 steps.
	if steps != 2 {
		t.Errorf("steps = %d, want 2", steps)
	}
}

func TestFindRollbackAncestor_MultiGlobGlobalOrder(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	// Two migration globs feed ONE ledger; step count must come from the
	// global basename order, not per-glob lengths. Edit the later (by
	// basename) file, which lives in the second glob.
	core := "database/migrations/*.php"
	mod := "app/Modules/*/database/migrations/*.php"
	anc := record("fpMulti", "ch", map[string]InputVector{
		core: {{Path: "database/migrations/2024_01_01_a.php", Hash: "h1"}},
		mod:  {{Path: "app/Modules/X/database/migrations/2024_02_01_b.php", Hash: "hOLD"}},
	})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{
			core: {{Path: "database/migrations/2024_01_01_a.php", Hash: "h1"}},
			mod:  {{Path: "app/Modules/X/database/migrations/2024_02_01_b.php", Hash: "hNEW"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected ancestor")
	}
	// Global order: 2024_01_01_a (match), 2024_02_01_b (differ) → unwind 1.
	if steps != 1 {
		t.Errorf("steps = %d, want 1", steps)
	}
}

func TestFindRollbackAncestor_RemovedFileReturnsNil(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	anc := record("fpRemoved", "ch", map[string]InputVector{migs: {
		{Path: "001.sql", Hash: "h1"},
		{Path: "002.sql", Hash: "h2"},
		{Path: "003.sql", Hash: "h3"},
	}})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	// Current drops 002: the branch has no down() for it, so a step
	// rollback would skip it silently and leave its schema applied.
	// The candidate must be rejected — prepare cold-rebuilds instead.
	got, _, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"},
			{Path: "003.sql", Hash: "h3"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("removed-file divergence must not be a rollback ancestor; got %s", got.Fingerprint)
	}
}

// Regression test for #118: a template built from a source AHEAD of
// the branch (migrations from origin/develop the branch does not
// contain) must not be selected. The old selection unwound those
// migrations in the branch's worktree, where their files do not exist
// — Laravel printed "Migration not found" and exited 0, the schema
// changes stayed applied, and the poisoned result was cached as a
// template reused by every later prepare.
func TestFindRollbackAncestor_AheadOfBranchReturnsNil(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "database/migrations/*.php"
	anc := record("fpAhead", "ch", map[string]InputVector{migs: {
		{Path: "2024_01_01_000001_create_users.php", Hash: "h1"},
		{Path: "2024_01_02_000001_create_orders.php", Hash: "h2"},
		{Path: "2026_09_21_091237_drop_render_transparent.php", Hash: "hdev1"},
		{Path: "2026_09_22_000001_create_webdav_pending_uploads_table.php", Hash: "hdev2"},
		{Path: "2026_09_22_125733_drop_settings_folder_templates_enabled_columns.php", Hash: "hdev3"},
	}})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	// The branch forked before the develop-only migrations and carries
	// one of its own; none of the develop tail exists on disk here.
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{migs: {
			{Path: "2024_01_01_000001_create_users.php", Hash: "h1"},
			{Path: "2024_01_02_000001_create_orders.php", Hash: "h2"},
			{Path: "2026_06_15_000000_branch_own_change.php", Hash: "hbranch"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("ahead-of-branch candidate must not be a rollback ancestor; got %s steps=%d", got.Fingerprint, steps)
	}
}

// A tail that mixes an editable migration with one the branch lacks is
// rejected wholesale: partial unwindability still leaves the unknown
// migration's schema baked in.
func TestFindRollbackAncestor_MixedTailReturnsNil(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	anc := record("fpMixed", "ch", map[string]InputVector{migs: {
		{Path: "001.sql", Hash: "h1"},
		{Path: "002.sql", Hash: "hOLD"},
		{Path: "003.sql", Hash: "h3"},
		{Path: "004_dev_only.sql", Hash: "hdev"},
	}})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"},
			{Path: "002.sql", Hash: "hNEW"},
			{Path: "003.sql", Hash: "h3"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("partially unwindable tail must not be a rollback ancestor; got %s", got.Fingerprint)
	}
}

func TestFindRollbackAncestor_PureAppendReturnsNil(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	anc := record("fpAppend", "ch", map[string]InputVector{migs: {
		{Path: "001.sql", Hash: "h1"},
		{Path: "002.sql", Hash: "h2"},
	}})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"},
			{Path: "002.sql", Hash: "h2"},
			{Path: "003.sql", Hash: "h3"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("pure append must not be a rollback ancestor; got %s steps=%d", got.Fingerprint, steps)
	}
}

func TestFindRollbackAncestor_ExactMatchReturnsNil(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	v := InputVector{{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "h2"}}
	if err := s.RecordSnapshot(ctx, record("fpSame", "ch", map[string]InputVector{migs: v})); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{migs: v})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("exact match must return nil; got %s", got.Fingerprint)
	}
}

func TestFindRollbackAncestor_PicksLongestCommonPrefix(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	// P diverges at index 2 (prefix 2, steps 1); Q diverges at index 1
	// (prefix 1, steps 2). Must pick P — fewest migrations to unwind.
	for _, r := range []SnapshotRecord{
		record("fpP", "ch", map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "h2"}, {Path: "003.sql", Hash: "hOLD"},
		}}),
		record("fpQ", "ch", map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "hOLD2"}, {Path: "003.sql", Hash: "hOLD"},
		}}),
	} {
		if err := s.RecordSnapshot(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"},
			{Path: "002.sql", Hash: "h2"},
			{Path: "003.sql", Hash: "hNEW"},
			{Path: "004.sql", Hash: "h4"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Fingerprint != "fpP" {
		t.Fatalf("want fpP (longest common prefix), got %v", got)
	}
	if steps != 1 {
		t.Errorf("steps = %d, want 1", steps)
	}
}

func TestFindRollbackAncestor_ExcludesDumpOnlyTemplate(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	// A dump-only marker row (empty Inputs) must never be offered.
	dumpOnly := SnapshotRecord{
		Fingerprint: "fpDump", Engine: "mysql", EngineVersion: "8.0",
		SourceDB: "src", TemplateName: "_tm_dump", DumpHash: "dh",
		LockfileHashes: map[string]string{DumpOnlyMarkerKey: "1"},
		Inputs:         map[string]InputVector{}, RepoID: 1,
	}
	if err := s.RecordSnapshot(ctx, dumpOnly); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil,
		map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "h2"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("dump-only template must be excluded from rollback ancestors; got %s", got.Fingerprint)
	}
}

func TestMergeVectorsByBasename(t *testing.T) {
	merged := mergeVectorsByBasename(map[string]InputVector{
		"a/*.sql": {{Path: "a/2024_03.sql", Hash: "h3"}, {Path: "a/2024_01.sql", Hash: "h1"}},
		"b/*.sql": {{Path: "b/2024_02.sql", Hash: "h2"}},
	})
	wantOrder := []string{"a/2024_01.sql", "b/2024_02.sql", "a/2024_03.sql"}
	if len(merged) != len(wantOrder) {
		t.Fatalf("len = %d, want %d", len(merged), len(wantOrder))
	}
	for i, w := range wantOrder {
		if merged[i].Path != w {
			t.Errorf("merged[%d] = %s, want %s", i, merged[i].Path, w)
		}
	}
}

func TestCommonPrefixLen(t *testing.T) {
	a := []FileHash{{Path: "1", Hash: "h1"}, {Path: "2", Hash: "h2"}, {Path: "3", Hash: "h3"}}
	cases := []struct {
		b    []FileHash
		want int
	}{
		{a, 3},
		{[]FileHash{{Path: "1", Hash: "h1"}, {Path: "2", Hash: "hX"}}, 1},
		{[]FileHash{{Path: "1", Hash: "h1"}}, 1},
		{[]FileHash{{Path: "X", Hash: "h1"}}, 0},
		{nil, 0},
	}
	for i, c := range cases {
		if got := commonPrefixLen(a, c.b); got != c.want {
			t.Errorf("case %d: commonPrefixLen = %d, want %d", i, got, c.want)
		}
	}
}

func TestPathBase(t *testing.T) {
	for in, want := range map[string]string{
		"a/b/c.sql":   "c.sql",
		"c.sql":       "c.sql",
		"a\\b\\c.sql": "c.sql",
		"":            "",
	} {
		if got := pathBase(in); got != want {
			t.Errorf("pathBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// Regression: a composer.lock bump with identical migrations sorted the
// lockfile after every timestamped migration, so the merged order read
// it as one diverging step and the rebuild unwound the newest real
// migration. Declared `kind: lockfile`, it is not a step: the ancestor
// comes back with zero steps for a forward-only migrate.
func TestFindRollbackAncestor_LockfileOnlyChangeIsZeroSteps(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "database/migrations/**/*.php"
	lock := "composer.lock"
	ledger := InputVector{
		{Path: "database/migrations/2026_11_04_150019_add_style_columns.php", Hash: "h1"},
		{Path: "database/migrations/2026_11_05_090000_add_cdn_hash_date_index.php", Hash: "h2"},
	}
	anc := record("fpLock", "ch", map[string]InputVector{
		migs: ledger,
		lock: {{Path: "composer.lock", Hash: "hOLD"}},
	})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]struct{}{lock: {}},
		map[string]InputVector{
			migs: ledger,
			lock: {{Path: "composer.lock", Hash: "hNEW"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Fingerprint != "fpLock" {
		t.Fatalf("want fpLock ancestor, got %v", got)
	}
	if steps != 0 {
		t.Errorf("steps = %d, want 0 (a lockfile is never unwound)", steps)
	}
}

func TestFindRollbackAncestor_LockfileChangeWithAppendIsZeroSteps(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	lock := "composer.lock"
	anc := record("fpLockAppend", "ch", map[string]InputVector{
		migs: {{Path: "001.sql", Hash: "h1"}},
		lock: {{Path: "composer.lock", Hash: "hOLD"}},
	})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]struct{}{lock: {}},
		map[string]InputVector{
			migs: {{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "h2"}},
			lock: {{Path: "composer.lock", Hash: "hNEW"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || steps != 0 {
		t.Fatalf("want fpLockAppend with 0 steps, got %v steps=%d", got, steps)
	}
}

func TestFindRollbackAncestor_LockfileNotCountedInEditSteps(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	lock := "composer.lock"
	anc := record("fpLockEdit", "ch", map[string]InputVector{
		migs: {{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "hOLD"}},
		lock: {{Path: "composer.lock", Hash: "hOLD"}},
	})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]struct{}{lock: {}},
		map[string]InputVector{
			migs: {{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "hNEW"}},
			lock: {{Path: "composer.lock", Hash: "hNEW"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || steps != 1 {
		t.Fatalf("want fpLockEdit with 1 step (002 only), got %v steps=%d", got, steps)
	}
}

func TestFindRollbackAncestor_UnchangedLockfileWithAppendReturnsNil(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	lock := "composer.lock"
	lockVec := InputVector{{Path: "composer.lock", Hash: "h"}}
	anc := record("fpLockSame", "ch", map[string]InputVector{
		migs: {{Path: "001.sql", Hash: "h1"}},
		lock: lockVec,
	})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]struct{}{lock: {}},
		map[string]InputVector{
			migs: {{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "h2"}},
			lock: lockVec,
		})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("pure append with unchanged lockfile is FindAncestorSnapshot's case; got %s", got.Fingerprint)
	}
}

func TestFindRollbackAncestor_PrefersZeroStepsOnEqualPrefix(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	lock := "composer.lock"
	for _, r := range []SnapshotRecord{
		record("fpTail", "ch", map[string]InputVector{
			migs: {{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "hOLD"}},
			lock: {{Path: "composer.lock", Hash: "hNEW"}},
		}),
		record("fpPrefix", "ch", map[string]InputVector{
			migs: {{Path: "001.sql", Hash: "h1"}},
			lock: {{Path: "composer.lock", Hash: "hOLD"}},
		}),
	} {
		if err := s.RecordSnapshot(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, steps, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]struct{}{lock: {}},
		map[string]InputVector{
			migs: {{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "hNEW"}},
			lock: {{Path: "composer.lock", Hash: "hNEW"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Fingerprint != "fpPrefix" || steps != 0 {
		t.Fatalf("want fpPrefix with 0 steps, got %v steps=%d", got, steps)
	}
}
