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

// findRollback runs FindRollbackAncestor for repo 1's mysql 8.0 "dh"/"ch"
// candidates and unpacks the pick into its snapshot and step count.
func findRollback(
	ctx context.Context,
	s *Store,
	lockfileGlobs map[string]struct{},
	current map[string]InputVector,
) (*SnapshotRecord, int, error) {
	got, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", lockfileGlobs, current)
	if err != nil || got == nil {
		return nil, 0, err
	}
	return &got.Snapshot, got.Steps, nil
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
	got, steps, err := findRollback(ctx, s, nil,
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
	got, steps, err := findRollback(ctx, s, nil,
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
	got, _, err := findRollback(ctx, s, nil,
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
	got, steps, err := findRollback(ctx, s, nil,
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
	got, _, err := findRollback(ctx, s, nil,
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
	got, steps, err := findRollback(ctx, s, nil,
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
	got, _, err := findRollback(ctx, s, nil,
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
	got, steps, err := findRollback(ctx, s, nil,
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
	got, _, err := findRollback(ctx, s, nil,
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
	got, steps, err := findRollback(ctx, s,
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
	got, steps, err := findRollback(ctx, s,
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
	got, steps, err := findRollback(ctx, s,
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
	got, _, err := findRollback(ctx, s,
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
	got, steps, err := findRollback(ctx, s,
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

// Regression for #185: merging develop into a branch inserted migrations
// dated earlier than the branch's own. The pre-merge template diverged
// from the merged inputs at the first inserted file, so every template
// entry after it counted as a step (34) and the rollback ran a lossy
// down() whose re-migrate failed. An inserted file is pending, not
// changed: nothing needs unwinding.
func TestFindRollbackAncestor_InsertedEarlierMigrationsAreZeroSteps(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "database/migrations/*.php"
	anc := record("fpPreMerge", "ch", map[string]InputVector{migs: {
		{Path: "database/migrations/2026_09_01_000000_create_clients.php", Hash: "h1"},
		{Path: "database/migrations/2026_10_14_123257_drop_moved_and_dead_columns.php", Hash: "hb1"},
		{Path: "database/migrations/2026_10_30_000000_branch_followup.php", Hash: "hb2"},
	}})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, steps, err := findRollback(ctx, s, nil,
		map[string]InputVector{migs: {
			{Path: "database/migrations/2026_09_01_000000_create_clients.php", Hash: "h1"},
			{Path: "database/migrations/2026_10_01_000000_develop_a.php", Hash: "hd1"},
			{Path: "database/migrations/2026_10_14_123257_drop_moved_and_dead_columns.php", Hash: "hb1"},
			{Path: "database/migrations/2026_10_20_000000_develop_b.php", Hash: "hd2"},
			{Path: "database/migrations/2026_10_30_000000_branch_followup.php", Hash: "hb2"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Fingerprint != "fpPreMerge" {
		t.Fatalf("want fpPreMerge ancestor, got %v", got)
	}
	if steps != 0 {
		t.Errorf("steps = %d, want 0 (inserted migrations are applied forward, never unwound)", steps)
	}
}

func TestFindRollbackAncestor_EditAfterInsertionsCountsCandidateEntriesOnly(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	anc := record("fpInsertEdit", "ch", map[string]InputVector{migs: {
		{Path: "010.sql", Hash: "h10"},
		{Path: "030.sql", Hash: "h30"},
		{Path: "050.sql", Hash: "hOLD"},
		{Path: "070.sql", Hash: "h70"},
	}})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, steps, err := findRollback(ctx, s, nil,
		map[string]InputVector{migs: {
			{Path: "010.sql", Hash: "h10"},
			{Path: "020.sql", Hash: "h20"},
			{Path: "030.sql", Hash: "h30"},
			{Path: "040.sql", Hash: "h40"},
			{Path: "050.sql", Hash: "hNEW"},
			{Path: "060.sql", Hash: "h60"},
			{Path: "070.sql", Hash: "h70"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Fingerprint != "fpInsertEdit" {
		t.Fatalf("want fpInsertEdit ancestor, got %v", got)
	}
	if steps != 2 {
		t.Errorf("steps = %d, want 2 (050 edited + 070 after it; inserted 020/040/060 are not steps)", steps)
	}
}

func TestUnchangedLeadLen(t *testing.T) {
	cand := []FileHash{{Path: "1", Hash: "h1"}, {Path: "2", Hash: "h2"}, {Path: "3", Hash: "h3"}}
	cases := []struct {
		name    string
		current map[string]string
		want    int
	}{
		{"all held, insertions ignored", map[string]string{"0": "h0", "1": "h1", "2": "h2", "25": "h25", "3": "h3"}, 3},
		{"edited entry ends the run", map[string]string{"1": "h1", "2": "hX", "3": "h3"}, 1},
		{"removed entry ends the run", map[string]string{"1": "h1", "3": "h3"}, 1},
		{"first entry removed", map[string]string{"2": "h2", "3": "h3"}, 0},
	}
	for _, c := range cases {
		if got := unchangedLeadLen(cand, c.current); got != c.want {
			t.Errorf("%s: unchangedLeadLen = %d, want %d", c.name, got, c.want)
		}
	}
}

// Regression for #189: a forward-only rebuild (steps == 0) applies an
// inserted earlier-dated migration in a NEWER batch, which Laravel
// unwinds first. A later edit of the branch's own migration must unwind
// both, not just the latest-dated file.
func TestFindRollbackAncestor_ForwardOnlyTemplateUnwindsAcrossBatches(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "database/migrations/*.php"
	base := FileHash{Path: "database/migrations/2026_09_01_base.php", Hash: "h1"}
	develop := FileHash{Path: "database/migrations/2026_10_01_develop.php", Hash: "hd"}
	branch := FileHash{Path: "database/migrations/2026_10_14_branch.php", Hash: "hb"}

	if err := s.RecordSnapshot(ctx, record("fpCold", "ch", map[string]InputVector{migs: {base, branch}})); err != nil {
		t.Fatal(err)
	}
	merged := map[string]InputVector{migs: {base, develop, branch}}
	plan, err := s.FindRollbackAncestor(ctx, 1, "mysql", "8.0", "dh", "ch", nil, merged)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.Steps != 0 {
		t.Fatalf("want a forward-only plan off fpCold, got %+v", plan)
	}
	forward := record("fpForward", "ch", merged)
	forward.Ledger = plan.Kept.Advance(merged)
	if err := s.RecordSnapshot(ctx, forward); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot(ctx, "fpCold"); err != nil {
		t.Fatal(err)
	}

	edited := map[string]InputVector{migs: {base, develop, {Path: branch.Path, Hash: "hbNEW"}}}
	got, steps, err := findRollback(ctx, s, nil, edited)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Fingerprint != "fpForward" {
		t.Fatalf("want fpForward ancestor, got %v", got)
	}
	if steps != 2 {
		t.Errorf("steps = %d, want 2 (develop sits in the newest batch, so Laravel unwinds it before the edited branch migration)", steps)
	}
}

// Regression for #189: an append build with several globs records the
// appended file in a later batch even though it sorts before files of
// the other glob.
func TestFindRollbackAncestor_MultiGlobAppendUnwindsAcrossBatches(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	core := "database/migrations/*.php"
	mod := "app/Modules/*/database/migrations/*.php"
	a := FileHash{Path: "database/migrations/2024_01_01_a.php", Hash: "ha"}
	b := FileHash{Path: "database/migrations/2024_02_01_b.php", Hash: "hb"}
	c := FileHash{Path: "app/Modules/X/database/migrations/2024_03_01_c.php", Hash: "hc"}

	cold := map[string]InputVector{core: {a}, mod: {c}}
	appended := map[string]InputVector{core: {a, b}, mod: {c}}
	tpl := record("fpAppend", "ch", appended)
	tpl.Ledger = NewAppliedLedger(cold).Advance(appended)
	if err := s.RecordSnapshot(ctx, tpl); err != nil {
		t.Fatal(err)
	}

	got, steps, err := findRollback(ctx, s, nil, map[string]InputVector{
		core: {a, b},
		mod:  {{Path: c.Path, Hash: "hcNEW"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected fpAppend ancestor")
	}
	if steps != 2 {
		t.Errorf("steps = %d, want 2 (b is in the newest batch, c in the one before)", steps)
	}
}

func TestFindRollbackAncestor_UnknownLedgerIsSkipped(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "migrations/*.sql"
	anc := record("fpLegacy", "ch", map[string]InputVector{migs: {
		{Path: "001.sql", Hash: "h1"},
		{Path: "002.sql", Hash: "hOLD"},
	}})
	anc.Ledger = nil
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, _, err := findRollback(ctx, s, nil, map[string]InputVector{migs: {
		{Path: "001.sql", Hash: "h1"},
		{Path: "002.sql", Hash: "hNEW"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a template whose batches are unknown must not be a rollback ancestor; got %s", got.Fingerprint)
	}
}

func TestAgreedUnwindLen(t *testing.T) {
	fh := func(p string) FileHash { return FileHash{Path: p} }
	cases := []struct {
		name            string
		byName, byBatch []FileHash
		minSteps, want  int
	}{
		{"single batch keeps the minimum", []FileHash{fh("1"), fh("2"), fh("3")}, []FileHash{fh("1"), fh("2"), fh("3")}, 1, 1},
		{"zero steps stays zero", []FileHash{fh("1"), fh("2")}, []FileHash{fh("2"), fh("1")}, 0, 0},
		{"later batch widens the tail", []FileHash{fh("1"), fh("2"), fh("3")}, []FileHash{fh("1"), fh("3"), fh("2")}, 1, 2},
		{"disagreement down to the root", []FileHash{fh("1"), fh("2"), fh("3")}, []FileHash{fh("2"), fh("3"), fh("1")}, 1, 3},
	}
	for _, c := range cases {
		if got := agreedUnwindLen(c.byName, c.byBatch, c.minSteps); got != c.want {
			t.Errorf("%s: agreedUnwindLen = %d, want %d", c.name, got, c.want)
		}
	}
}
