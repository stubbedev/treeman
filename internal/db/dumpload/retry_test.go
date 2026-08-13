package dumpload

import (
	"context"
	"errors"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
)

func deadlock() error {
	return &gomysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}
}

// A deadlock under autocommit retries until the statement lands.
func TestRetryingExecRetriesDeadlock(t *testing.T) {
	calls := 0
	exec := retryingExec(context.Background(), func(string) error {
		calls++
		if calls < 3 {
			return deadlock()
		}
		return nil
	})
	if err := exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("want nil after retry, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 attempts, got %d", calls)
	}
}

// A non-lock error fails on the first attempt — no pointless retry of a
// syntax error or a missing table.
func TestRetryingExecDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	exec := retryingExec(context.Background(), func(string) error {
		calls++
		return errors.New("Error 1049 (42000): Unknown database 'x'")
	})
	if err := exec("INSERT INTO t VALUES (1)"); err == nil {
		t.Fatal("want error")
	}
	if calls != 1 {
		t.Fatalf("want 1 attempt, got %d", calls)
	}
}

// Inside an explicit transaction the deadlock rolled back every earlier
// statement, so retrying just this one would commit a partial dump.
func TestRetryingExecDoesNotRetryInsideTransaction(t *testing.T) {
	calls := 0
	exec := retryingExec(context.Background(), func(stmt string) error {
		calls++
		if stmt == "START TRANSACTION" {
			return nil
		}
		return deadlock()
	})
	if err := exec("START TRANSACTION"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := exec("INSERT INTO t VALUES (1)"); err == nil {
		t.Fatal("want error")
	}
	if calls != 2 {
		t.Fatalf("want 2 attempts (begin + one insert), got %d", calls)
	}
}

// COMMIT reopens the autocommit world, so retries resume afterwards.
func TestRetryingExecRetriesAfterCommit(t *testing.T) {
	calls := 0
	exec := retryingExec(context.Background(), func(string) error {
		calls++
		return nil
	})
	for _, s := range []string{"BEGIN", "INSERT INTO t VALUES (1)", "COMMIT"} {
		if err := exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	deadlocks := 0
	exec2 := retryingExec(context.Background(), func(string) error {
		deadlocks++
		if deadlocks < 2 {
			return deadlock()
		}
		return nil
	})
	if err := exec2("INSERT INTO t VALUES (2)"); err != nil {
		t.Fatalf("want retry after commit, got %v", err)
	}
	if deadlocks != 2 {
		t.Fatalf("want 2 attempts, got %d", deadlocks)
	}
}

// A cancelled context aborts the backoff instead of sleeping it out.
func TestRetryingExecHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exec := retryingExec(ctx, func(string) error { return deadlock() })
	if err := exec("INSERT INTO t VALUES (1)"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
