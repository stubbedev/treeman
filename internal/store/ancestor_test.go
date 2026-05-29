package store

import (
	"context"
	"path/filepath"
	"testing"
)

func record(fp, cmds string, inputs map[string]InputVector) SnapshotRecord {
	return SnapshotRecord{
		Fingerprint:    fp,
		Engine:         "mysql",
		EngineVersion:  "8.0",
		SourceDB:       "src",
		TemplateName:   "_tm_" + fp,
		MigrationsHash: "",
		DumpHash:       "dh",
		LockfileHashes: map[string]string{CommandsHashKey: cmds},
		Inputs:         inputs,
		RepoID:         1,
	}
}

func TestFindAncestorSnapshot_PicksLongestStrictPrefix(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.EnsureRepo(ctx, "/r", "r"); err != nil {
		t.Fatal(err)
	}

	migs := "migrations/*.sql"
	vec := func(n int) InputVector {
		out := make(InputVector, n)
		for i := range n {
			out[i] = FileHash{Path: "migrations/m" + string('a'+rune(i)) + ".sql", Hash: "h" + string('a'+rune(i))}
		}
		return out
	}

	// Three cached snapshots covering 1, 2, and 3 migrations
	// respectively, plus an unrelated one with a different commands
	// hash that must be rejected.
	for _, r := range []SnapshotRecord{
		record("fp1", "ch", map[string]InputVector{migs: vec(1)}),
		record("fp2", "ch", map[string]InputVector{migs: vec(2)}),
		record("fp3", "ch", map[string]InputVector{migs: vec(3)}),
		record("fpOtherCmds", "DIFFERENT", map[string]InputVector{migs: vec(2)}),
	} {
		if err := s.RecordSnapshot(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// Current run has 4 migrations; ancestor must be fp3 (the longest
	// strict prefix). fpOtherCmds is rejected for the commands mismatch.
	got, err := s.FindAncestorSnapshot(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]InputVector{migs: vec(4)})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected an ancestor")
	}
	if got.Fingerprint != "fp3" {
		t.Errorf("longest-prefix ancestor = %s, want fp3", got.Fingerprint)
	}
}

func TestFindAncestorSnapshot_RejectsBranchDivergence(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.EnsureRepo(ctx, "/r", "r"); err != nil {
		t.Fatal(err)
	}

	migs := "migrations/*.sql"
	// Ancestor has [m1, mX] (some edited or removed file); current has
	// [m1, m2]. Not a prefix — must NOT match.
	anc := record("fpDiverge", "ch", map[string]InputVector{migs: {
		{Path: "001.sql", Hash: "h1"},
		{Path: "002.sql", Hash: "hX"},
	}})
	if err := s.RecordSnapshot(ctx, anc); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindAncestorSnapshot(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]InputVector{migs: {
			{Path: "001.sql", Hash: "h1"},
			{Path: "002.sql", Hash: "h2"}, // hash differs from ancestor's
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected NO ancestor (divergent input); got %s", got.Fingerprint)
	}
}

func TestFindAncestorSnapshot_ExactMatchIsNotAnAncestor(t *testing.T) {
	// An exact-vector match is the LookupSnapshot cache-hit path, not
	// an incremental ancestor. FindAncestorSnapshot must return nil
	// in that case so the caller takes the cache-hit branch instead.
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.EnsureRepo(ctx, "/r", "r"); err != nil {
		t.Fatal(err)
	}

	migs := "migrations/*.sql"
	v := InputVector{{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "h2"}}
	if err := s.RecordSnapshot(ctx, record("fpSame", "ch", map[string]InputVector{migs: v})); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindAncestorSnapshot(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]InputVector{migs: v})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("exact match must return nil (cache-hit path, not incremental); got %s", got.Fingerprint)
	}
}

func TestFindAncestorSnapshot_RejectsCandidateWithExtraInput(t *testing.T) {
	// A candidate with an input the current run lacks can't be an
	// ancestor — the current run's state doesn't even include that
	// input glob, so there's nothing to extend from.
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.EnsureRepo(ctx, "/r", "r"); err != nil {
		t.Fatal(err)
	}

	migs := "migrations/*.sql"
	extras := "extras/*.sql"
	if err := s.RecordSnapshot(ctx, record("fpExtra", "ch", map[string]InputVector{
		migs:   {{Path: "001.sql", Hash: "h1"}},
		extras: {{Path: "e.sql", Hash: "he"}},
	})); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindAncestorSnapshot(ctx, 1, "mysql", "8.0", "dh", "ch",
		map[string]InputVector{migs: {{Path: "001.sql", Hash: "h1"}, {Path: "002.sql", Hash: "h2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("candidate with extra input must not match; got %s", got.Fingerprint)
	}
}
