package prepare

import (
	"context"
	"testing"
)

// adopt (active exists, no marker) must record the captured durable in the
// branch_durables tracking table so the orphan sweep can later reclaim it.
func TestAdoptRecordsBranchDurable(t *testing.T) {
	ctx := context.Background()
	f := newBSFixture(t)
	f.set(f.active, map[string]string{"seed": "1"}) // pre-existing, unmarked

	f.run("develop")

	rows, err := f.st.ListBranchDurables(ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d durable rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Branch != "develop" || r.DurableName != f.durable("develop") {
		t.Fatalf("recorded %+v, want branch=develop durable=%s", r, f.durable("develop"))
	}
	if r.DBKey != f.active || r.Engine != "mysql" || r.WorktreeID != f.worktreeID {
		t.Errorf("provenance off: %+v (active=%s wt=%d)", r, f.active, f.worktreeID)
	}
}

// A branch switch captures the OLD branch's data into its durable copy and
// records it — so both branches' durables are tracked after a swap.
func TestSwapRecordsOldBranchDurable(t *testing.T) {
	ctx := context.Background()
	f := newBSFixture(t)
	f.set(f.active, map[string]string{"seed": "1"})

	f.run("develop")            // adopt → records develop durable
	f.write(f.active, "x", "1") // dirty active so the swap actually captures
	f.run("feature/y")          // swap → captures + records develop durable

	rows, err := f.st.ListBranchDurables(ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	byBranch := map[string]string{}
	for _, r := range rows {
		byBranch[r.Branch] = r.DurableName
	}
	if byBranch["develop"] != f.durable("develop") {
		t.Fatalf("develop durable not tracked after swap: %v", byBranch)
	}
}
