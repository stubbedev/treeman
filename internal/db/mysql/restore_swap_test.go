package mysql

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/internal/db/restorestage"
)

func TestRenameSwapStatementMovesTargetOutBeforeMovingCloneIn(t *testing.T) {
	stage := restorestage.Stage{Incoming: "_tmrs_1_0123456789abcdef_n", Retired: "_tmrs_1_0123456789abcdef_o"}
	got, err := renameSwapStatement("app_test_6", stage, []string{"appearances", "users"}, []string{"appearances", "orders"})
	if err != nil {
		t.Fatal(err)
	}
	want := "RENAME TABLE " +
		"`app_test_6`.`appearances` TO `_tmrs_1_0123456789abcdef_o`.`appearances`, " +
		"`app_test_6`.`users` TO `_tmrs_1_0123456789abcdef_o`.`users`, " +
		"`_tmrs_1_0123456789abcdef_n`.`appearances` TO `app_test_6`.`appearances`, " +
		"`_tmrs_1_0123456789abcdef_n`.`orders` TO `app_test_6`.`orders`"
	if got != want {
		t.Fatalf("got\n  %s\nwant\n  %s", got, want)
	}
}

func TestRenameSwapStatementRejectsUnsafeNames(t *testing.T) {
	stage := restorestage.Stage{Incoming: "_tmrs_1_0123456789abcdef_n", Retired: "_tmrs_1_0123456789abcdef_o"}
	if _, err := renameSwapStatement("app", stage, []string{"a`b"}, nil); err == nil {
		t.Fatal("a table name with a backtick was accepted")
	}
}

func TestSwapErrorNamesConcurrentChangesToTheTarget(t *testing.T) {
	for _, n := range []uint16{erBadDB, erTableExists, erNoSuchTable, erTrgInWrongSchema} {
		cause := &gomysql.MySQLError{Number: n, Message: "boom"}
		err := swapError("app_test_6", cause)
		if !strings.Contains(err.Error(), "another client created or dropped objects in app_test_6") {
			t.Errorf("error %d: %q does not say another client touched the target", n, err)
		}
		if !errors.Is(err, cause) {
			t.Errorf("error %d: cause not wrapped", n)
		}
	}
	other := &gomysql.MySQLError{Number: 1045, Message: "access denied"}
	if err := swapError("app_test_6", other); err != other {
		t.Errorf("unrelated error was rewritten: %v", err)
	}
}

func TestDiffNames(t *testing.T) {
	extra, missing := diffNames([]string{"a", "b", "laravel_migrations"}, []string{"a", "b", "c"})
	if !reflect.DeepEqual(extra, []string{"laravel_migrations"}) || !reflect.DeepEqual(missing, []string{"c"}) {
		t.Fatalf("extra=%v missing=%v", extra, missing)
	}
	extra, missing = diffNames([]string{"a"}, []string{"a"})
	if extra != nil || missing != nil {
		t.Fatalf("identical sets: extra=%v missing=%v", extra, missing)
	}
}

func TestDropDictionaryScript(t *testing.T) {
	got, err := dropDictionaryScript("app", []dictionaryObject{
		{kind: dictionaryTrigger, name: "t1"},
		{kind: dictionaryView, name: "v1"},
		{kind: dictionaryProcedure, name: "p1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "DROP TRIGGER IF EXISTS `app`.`t1`;\nDROP VIEW IF EXISTS `app`.`v1`;\nDROP PROCEDURE IF EXISTS `app`.`p1`;\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseDictionaryKindRejectsUnknownKinds(t *testing.T) {
	for _, k := range []string{"TRIGGER", "VIEW", "PROCEDURE", "FUNCTION", "EVENT"} {
		if _, err := parseDictionaryKind(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	if _, err := parseDictionaryKind("TABLE"); err == nil {
		t.Error("TABLE was accepted as a dictionary-only object")
	}
}
