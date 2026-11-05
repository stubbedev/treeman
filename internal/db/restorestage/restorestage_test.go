package restorestage

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/db/ident"
)

func TestStageNamesArePrivateAndRecognisable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, b := New(now), New(now)
	if a.Incoming == b.Incoming {
		t.Fatalf("two restores share the stage %q", a.Incoming)
	}
	if a.Incoming == a.Retired {
		t.Fatalf("incoming and retired share %q", a.Incoming)
	}
	for _, n := range []string{a.Incoming, a.Retired} {
		if err := ident.ValidateMySQL(n); err != nil {
			t.Errorf("stage name %q is not a valid MySQL identifier: %v", n, err)
		}
		if err := ident.ValidatePostgres(n); err != nil {
			t.Errorf("stage name %q is not a valid Postgres identifier: %v", n, err)
		}
		if len(n) > 63 {
			t.Errorf("stage name %q exceeds Postgres' 63-byte identifier limit", n)
		}
		if !Is(n) {
			t.Errorf("stage name %q is not recognised as a restore stage", n)
		}
		if strings.HasPrefix(n, "_tm_") {
			t.Errorf("stage name %q falls in the _tm_ template namespace the orphan probe audits", n)
		}
	}
}

func TestStaleReapsOnlyOldStages(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := New(now.Add(-MaxAge - time.Hour))
	live := New(now.Add(-time.Hour))
	names := []string{
		old.Incoming, live.Incoming, old.Retired, live.Retired,
		"_tmrs_app", "_tmrs_" + strconv.FormatInt(now.Add(-48*time.Hour).Unix(), 16) + "_x_n",
	}
	got := Stale(names, now)
	want := []string{old.Incoming, old.Retired}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stale stages = %v, want %v", got, want)
	}
}
