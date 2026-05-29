package framework

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
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
	_ = os.WriteFile(filepath.Join(d, "package.json"), []byte(`{"name":"plain"}`), 0o644)
	_ = os.WriteFile(filepath.Join(d, "yarn.lock"), nil, 0o644)
	r := DefaultRegistry()
	detected := r.DetectAll(d)
	names := make([]string, 0, len(detected))
	for _, s := range detected {
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
	_ = os.WriteFile(filepath.Join(d, "package.json"), []byte(`{"name":"app"}`), 0o644)
	_ = os.WriteFile(filepath.Join(d, "data-source.ts"), []byte("export default {}"), 0o644)
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
	_ = os.WriteFile(filepath.Join(d, "knexfile.ts"), []byte("export default {}"), 0o644)
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
	_ = os.WriteFile(filepath.Join(d, "drizzle.config.mjs"), []byte("export default {}"), 0o644)
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

// RegistryFor must map every field of a user-defined `frameworks:`
// entry onto the detector Spec — markers, migration_dirs, file_pattern,
// hash_mode, lockfiles, engine_hint. The e2e proves detection; this
// pins the per-field translation so a renamed/dropped field is caught.
func TestRegistryForMapsCustomFrameworkFields(t *testing.T) {
	cfg := &config.Config{
		Frameworks: map[string]config.CustomFramework{
			"acme-migrate": {
				Markers:       []string{"acme.config"},
				MigrationDirs: []string{"db/acme"},
				FilePattern:   "*.sql",
				HashMode:      "filename",
				Lockfiles:     []string{"acme.lock"},
				EngineHint:    "postgres",
			},
		},
	}
	r := RegistryFor(cfg)

	var got *Spec
	for i := range r.Specs {
		if r.Specs[i].Name == "acme-migrate" {
			got = &r.Specs[i]
			break
		}
	}
	if got == nil {
		t.Fatal("custom framework 'acme-migrate' not present in registry")
	}
	if !reflect.DeepEqual(got.Markers, []string{"acme.config"}) {
		t.Errorf("Markers = %v, want [acme.config]", got.Markers)
	}
	if !reflect.DeepEqual(got.MigrationDirs, []string{"db/acme"}) {
		t.Errorf("MigrationDirs = %v, want [db/acme]", got.MigrationDirs)
	}
	if !reflect.DeepEqual(got.FileGlobs, []string{"*.sql"}) {
		t.Errorf("FileGlobs = %v, want [*.sql] (from file_pattern)", got.FileGlobs)
	}
	if got.HashMode != HashFilename {
		t.Errorf("HashMode = %q, want filename", got.HashMode)
	}
	if !reflect.DeepEqual(got.Lockfiles, []string{"acme.lock"}) {
		t.Errorf("Lockfiles = %v, want [acme.lock]", got.Lockfiles)
	}
	if got.EngineHint != "postgres" {
		t.Errorf("EngineHint = %q, want postgres", got.EngineHint)
	}

	// Built-ins must survive the merge (custom frameworks are additive).
	if len(r.Specs) <= len(DefaultRegistry().Specs) {
		t.Errorf("custom registry should be built-ins + 1, got %d vs default %d",
			len(r.Specs), len(DefaultRegistry().Specs))
	}
}
