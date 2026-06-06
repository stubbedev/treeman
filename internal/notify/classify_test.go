package notify

import (
	"context"
	"testing"
)

func TestBucketMapsLifecycleEvents(t *testing.T) {
	cases := []struct {
		eventType string
		level     string
		want      string
	}{
		{"worktree:create:start", "info", BucketUp},
		{"worktree:create:end", "info", BucketStable},
		{"worktree:create:error", "error", BucketFailed},
		// wt_finalize at a non-error level is not a failure transition.
		{"worktree:create:error", "info", ""},
		{"worktree:delete:start", "info", BucketDown},
		{"worktree:reap:start", "info", BucketDown},
		// Terminal teardown completion is intentionally unmapped.
		{"worktree:delete:end", "info", ""},
		{"worktree:reap:end", "info", ""},
		// Unrelated events never notify.
		{"auto_fetch_done", "info", ""},
		{"config:reload", "info", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := Bucket(c.eventType, c.level); got != c.want {
			t.Errorf("Bucket(%q, %q) = %q, want %q", c.eventType, c.level, got, c.want)
		}
	}
}

func TestComposeIncludesRepoAndTarget(t *testing.T) {
	n := Compose(BucketStable, "myrepo", "feature/x")
	if n.Title != "treeman: ready" {
		t.Errorf("title = %q", n.Title)
	}
	if want := "myrepo · feature/x finished preparing"; n.Body != want {
		t.Errorf("body = %q, want %q", n.Body, want)
	}
	if n.Urgency != UrgencyNormal {
		t.Errorf("urgency = %q, want normal", n.Urgency)
	}
}

func TestComposeFailedIsCritical(t *testing.T) {
	n := Compose(BucketFailed, "myrepo", "feature/x")
	if n.Urgency != UrgencyCritical {
		t.Errorf("failed urgency = %q, want critical", n.Urgency)
	}
}

func TestComposeOmitsEmptyTargetAndRepo(t *testing.T) {
	n := Compose(BucketUp, "myrepo", "")
	if want := "myrepo is being prepared"; n.Body != want {
		t.Errorf("body with empty target = %q, want %q", n.Body, want)
	}
	n = Compose(BucketUp, "", "")
	if want := "worktree is being prepared"; n.Body != want {
		t.Errorf("body with empty repo+target = %q, want %q", n.Body, want)
	}
}

func TestNewSenderNoneIsUnavailable(t *testing.T) {
	s := NewSender("none")
	if s.Available() {
		t.Error("none backend reported available")
	}
	if err := s.Send(context.Background(), Notification{Title: "t", Body: "b"}); err != nil {
		t.Errorf("none.Send returned error: %v", err)
	}
}
