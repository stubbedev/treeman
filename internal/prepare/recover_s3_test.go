package prepare

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// TestRecoverStaleWorktreeSkipsS3 guards #22: stale-worktree recovery
// must not touch a branch_scoped S3 bucket. Object stores have no
// dump/migrate/seed (validate.go rejects them), so there is no
// half-applied state to recover — while for an enrolled main worktree
// the active bucket is the developer's live overlay bucket, and
// dropping it on an unrelated failure is data loss.
//
// The endpoint counts requests: any bucket op (reachability probe,
// HeadBucket, DeleteBucket) hits it, so zero requests is the assertion.
func TestRecoverStaleWorktreeSkipsS3(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, "/tmp/repo", "main_master", "master")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{S3: &config.S3Conn{
			Endpoint:     srv.URL,
			AccessKey:    "key",
			SecretKey:    "secret",
			UsePathStyle: true,
		}},
		Databases: []config.DatabaseConfig{
			{Engine: "s3", KeyPrefix: "dev-media", BranchScoped: true},
		},
	}
	RecoverStaleWorktree(ctx, cfg, "main_master", "/tmp/repo", repoID, wtID, st)

	if n := hits.Load(); n != 0 {
		t.Fatalf("recovery hit the S3 endpoint %d time(s); branch_scoped buckets must be left untouched", n)
	}
	events, err := st.QueryEvents(ctx, store.EventFilter{
		EventTypes: []string{store.EvtWorktreeRecoverDrop, store.EvtWorktreeRecoverError},
		WorktreeID: wtID,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no recover events for s3; got %q", events[0].Message)
	}
}
