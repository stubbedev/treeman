package engine

import "testing"

// TestCanonical locks the engine-family mapping that `db reset
// --engine` filtering relies on: aliases collapse to the canonical
// Family label, unrecognised strings report ok=false. Moved here
// from internal/prepare so the alias mapping has a test in its
// owner package (prepare.CanonicalEngine is now a thin wrapper).
func TestCanonical(t *testing.T) {
	cases := map[string]struct {
		want Family
		ok   bool
	}{
		"mysql":         {FamilyMySQL, true},
		"mariadb":       {FamilyMySQL, true},
		"tidb":          {FamilyMySQL, true},
		"postgres":      {FamilyPostgres, true},
		"postgresql":    {FamilyPostgres, true},
		"mongodb":       {FamilyMongo, true},
		"redis":         {FamilyRedis, true},
		"elasticsearch": {FamilyES, true},
		"opensearch":    {FamilyES, true},
		"s3":            {FamilyS3, true},
		"sqlite":        {"", false},
		"":              {"", false},
	}
	for in, want := range cases {
		got, ok := Canonical(in)
		if ok != want.ok || (ok && got != want.want) {
			t.Errorf("Canonical(%q) = (%q, %t), want (%q, %t)", in, got, ok, want.want, want.ok)
		}
	}
}

// TestFamilyScope locks the scope-kind mapping so validate.go's
// prefix-vs-name template enforcement and prepare.go's branch
// adapter both read the same value.
func TestFamilyScope(t *testing.T) {
	cases := map[Family]Scope{
		FamilyMySQL:    ScopeName,
		FamilyPostgres: ScopeName,
		FamilyMongo:    ScopeName,
		FamilyRedis:    ScopePrefix,
		FamilyES:       ScopePrefix,
		FamilyS3:       ScopePrefix,
	}
	for fam, want := range cases {
		if got := fam.Scope(); got != want {
			t.Errorf("%s.Scope() = %v, want %v", fam, got, want)
		}
	}
}

// TestKnownCoversCanonical asserts every alias listed in Known
// rounds through Canonical -> ok=true. Drift guard: a new alias
// added to Known without a Canonical arm fails this immediately.
func TestKnownCoversCanonical(t *testing.T) {
	for _, eng := range Known {
		if _, ok := Canonical(eng); !ok {
			t.Errorf("Known engine %q has no Canonical mapping", eng)
		}
	}
}
