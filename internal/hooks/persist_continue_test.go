package hooks

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

func TestPersistContinueOnError(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional", true: "mixed"}[mixed], func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			repoID, _ := st.EnsureRepo(ctx, "/repo", "repo")
			wtID, _ := st.EnsureWorktree(ctx, repoID, "/repo/wt", "slug", "feature")
			out := RunOutcome{
				Groups: []GroupOutcome{
					{
						Command:         "npm ci",
						ExitCode:        7,
						ContinueOnError: true,
						StderrTail:      "invalid lockfile",
						LogBody:         []byte("invalid lockfile\n"),
					},
				},
			}
			wantLevel, wantFailed := store.LevelWarn, 1
			if mixed {
				out.Groups = append(out.Groups, GroupOutcome{Command: "composer install", ExitCode: 1})
				wantLevel, wantFailed = store.LevelError, 2
			}
			ids := PersistOutcome(ctx, st, repoID, wtID, "create-before-engines", 1000, 2000, out)
			if len(ids) != wantFailed || ids[0] == 0 {
				t.Fatalf("run IDs: %v", ids)
			}
			events, err := st.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtHooksEnd}, Limit: 1})
			if err != nil || len(events) != 1 {
				t.Fatalf("events=%v err=%v", events, err)
			}
			e := events[0]
			if e.Level != wantLevel || strings.Contains(e.Message, "continuing") == mixed {
				t.Fatalf("event: %+v", e)
			}
			var payload map[string]int
			if err := json.Unmarshal([]byte(e.PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["failed"] != wantFailed || payload["non_fatal"] != 1 || payload["max_exit"] != 7 {
				t.Fatalf("payload: %v", payload)
			}
			runs, err := st.QueryHookRuns(ctx, wtID, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range runs {
				if run.ID == ids[0] && (!run.ExitCode.Valid || run.ExitCode.Int64 != 7 || run.StderrTail != "invalid lockfile") {
					t.Fatalf("failure overwritten: %+v", run)
				}
			}
		})
	}
}
