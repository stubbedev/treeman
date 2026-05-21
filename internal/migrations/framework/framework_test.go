package framework

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectLaravelInTempdir(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "artisan"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := DefaultRegistry()
	found := false
	for _, s := range r.DetectAll(d) {
		if s.Name == "laravel" {
			found = true
		}
	}
	if !found {
		t.Error("laravel not detected")
	}
}

func TestPlainJsProjectDoesNotMatchOrms(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "package.json"), []byte(`{"name":"plain"}`), 0o644)
	os.WriteFile(filepath.Join(d, "yarn.lock"), nil, 0o644)
	r := DefaultRegistry()
	names := make([]string, 0)
	for _, s := range r.DetectAll(d) {
		names = append(names, s.Name)
	}
	for _, orm := range []string{"typeorm", "drizzle", "sequelize", "mikro-orm", "knex", "prisma"} {
		for _, n := range names {
			if n == orm {
				t.Errorf("%s matched plain JS project — detector too loose. matched: %v", orm, names)
			}
		}
	}
}

func TestTypeormMatchesOnlyOnDatasourceConfig(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "package.json"), []byte(`{"name":"app"}`), 0o644)
	os.WriteFile(filepath.Join(d, "data-source.ts"), []byte("export default {}"), 0o644)
	r := DefaultRegistry()
	hasTypeorm := false
	hasDrizzle := false
	for _, s := range r.DetectAll(d) {
		if s.Name == "typeorm" {
			hasTypeorm = true
		}
		if s.Name == "drizzle" {
			hasDrizzle = true
		}
	}
	if !hasTypeorm {
		t.Error("typeorm should match on data-source.ts")
	}
	if hasDrizzle {
		t.Error("drizzle should NOT match a typeorm-only project")
	}
}

func TestKnexMatchesTypescriptConfig(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "knexfile.ts"), []byte("export default {}"), 0o644)
	r := DefaultRegistry()
	hasKnex := false
	for _, s := range r.DetectAll(d) {
		if s.Name == "knex" {
			hasKnex = true
		}
	}
	if !hasKnex {
		t.Error("knex should match knexfile.ts")
	}
}

func TestDrizzleMatchesMjsConfig(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "drizzle.config.mjs"), []byte("export default {}"), 0o644)
	r := DefaultRegistry()
	hasDrizzle := false
	for _, s := range r.DetectAll(d) {
		if s.Name == "drizzle" {
			hasDrizzle = true
		}
	}
	if !hasDrizzle {
		t.Error("drizzle should match .mjs config")
	}
}

func TestLookupBuiltinKnownAndUnknown(t *testing.T) {
	if s, ok := LookupBuiltin("laravel"); !ok || s.Name != "laravel" || len(s.MigrationDirs) == 0 || len(s.FileGlobs) == 0 {
		t.Fatalf("laravel lookup failed: ok=%v spec=%+v", ok, s)
	}
	if _, ok := LookupBuiltin("not-a-framework"); ok {
		t.Fatalf("expected miss for unknown framework name")
	}
	if _, ok := LookupBuiltin(""); ok {
		t.Fatalf("expected miss for empty framework name")
	}
}

func TestBuiltinsHaveUniqueNames(t *testing.T) {
	r := DefaultRegistry()
	seen := map[string]bool{}
	for _, s := range r.Specs {
		if seen[s.Name] {
			t.Errorf("duplicate spec name: %s", s.Name)
		}
		seen[s.Name] = true
	}
}
