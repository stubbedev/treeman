package notify

import "github.com/stubbedev/treeman/internal/store"

// The four `treeman status` buckets a worktree can fall into. Kept in
// sync with the constants in cmd/treeman/cmd/status.go — these are the
// user-facing names the `notifications.events:` config lists.
const (
	BucketStable = "stable"
	BucketUp     = "up"
	BucketDown   = "down"
	BucketFailed = "failed"
)

// Bucket maps a store event (its type + level) to the status bucket a
// notification would announce, or "" when the event is not a lifecycle
// status transition worth notifying on.
//
// Mirrors deriveStatusBucket in cmd/treeman/cmd/status.go, but operates
// per-event (the moment of transition) rather than by replaying the
// newest event for a worktree:
//
//   - worktree:create:start             → up     (preparing began)
//   - worktree:create:end               → stable (ready)
//   - worktree:create:error (lvl=error) → failed (finalize errored)
//   - worktree:delete:start             → down   (teardown began)
//   - worktree:reap:start               → down
//
// The terminal teardown events (worktree:delete:end /
// worktree:reap:end) are intentionally unmapped: the worktree is gone,
// so "down" already fired at teardown start and a second banner for
// the completion would just be noise.
func Bucket(eventType, level string) string {
	switch eventType {
	case store.EvtWorktreeCreateStart:
		return BucketUp
	case store.EvtWorktreeCreateEnd:
		return BucketStable
	case store.EvtWorktreeCreateError:
		// Only ever written as the terminal error event (see
		// finalize.go / stale_finalize.go); gate on level so a future
		// non-error use can't masquerade as a failure.
		if level == "error" {
			return BucketFailed
		}
		return ""
	case store.EvtWorktreeDeleteStart, store.EvtWorktreeReapStart:
		return BucketDown
	default:
		return ""
	}
}

// urgencyForBucket maps a bucket to its notification urgency. Only a
// failed finalize is critical; everything else is informational.
func urgencyForBucket(bucket string) Urgency {
	if bucket == BucketFailed {
		return UrgencyCritical
	}
	return UrgencyNormal
}

// Compose builds the notification banner for a status transition.
// `repo` is the repository's display name and `target` is the worktree
// branch or slug (either may be empty, in which case it's omitted).
func Compose(bucket, repo, target string) Notification {
	subject := repo
	if target != "" {
		if subject != "" {
			subject += " · " + target
		} else {
			subject = target
		}
	}
	if subject == "" {
		subject = "worktree"
	}

	var title, body string
	switch bucket {
	case BucketStable:
		title = "treeman: ready"
		body = subject + " finished preparing"
	case BucketUp:
		title = "treeman: preparing"
		body = subject + " is being prepared"
	case BucketDown:
		title = "treeman: tearing down"
		body = subject + " is being torn down"
	case BucketFailed:
		title = "treeman: failed"
		body = subject + " failed to prepare"
	default:
		title = "treeman"
		body = subject
	}
	return Notification{Title: title, Body: body, Urgency: urgencyForBucket(bucket)}
}
