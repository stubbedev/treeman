package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSwapErrorNamesWhatHappenedToTheTarget(t *testing.T) {
	cases := map[string]string{
		sqlStateObjectInUse:        "other sessions are connected to app_test_6",
		sqlStateDuplicateDatabase:  "another client created or dropped app_test_6",
		sqlStateInvalidCatalogName: "another client created or dropped app_test_6",
	}
	for code, want := range cases {
		cause := &pgconn.PgError{Code: code, Message: "boom"}
		err := swapError("app_test_6", cause)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %q does not contain %q", code, err, want)
		}
		if !errors.Is(err, cause) {
			t.Errorf("%s: cause not wrapped", code)
		}
	}
	other := &pgconn.PgError{Code: "42501", Message: "permission denied"}
	if err := swapError("app_test_6", other); err.Error() != other.Error() {
		t.Errorf("unrelated error was rewritten: %v", err)
	}
}
