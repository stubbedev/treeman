package prepare

import (
	"reflect"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/migrations/framework"
)

func TestSpecFromYAML_MergesBuiltinLaravelDefaults(t *testing.T) {
	got := specFromYAML(config.MigrationSpec{Framework: "laravel"})
	want := framework.Spec{
		Name: "laravel",
		MigrationDirs: []string{
			"database/migrations",
			"app/Modules/*/Database/Migrations",
			"app/Modules/*/Database/migrations",
			"Modules/*/Database/Migrations",
			"Modules/*/Database/migrations",
		},
		FileGlobs: []string{"*.php"},
		Lockfiles: []string{"composer.lock"},
		HashMode:  framework.HashFilename,
		OnModify:  framework.OnRebuild,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("laravel defaults not hydrated:\n got=%#v\nwant=%#v", got, want)
	}
}

func TestSpecFromYAML_UserExtrasUnionAndDedup(t *testing.T) {
	got := specFromYAML(config.MigrationSpec{
		Framework: "laravel",
		MigrationDirs: []string{
			"database/migrations", // duplicates a built-in
			"custom/migrations",
		},
		FileGlobs: []string{"*.php"}, // duplicates a built-in
		Lockfiles: []string{"composer.lock", "extra.lock"},
	})
	wantDirs := []string{
		"database/migrations",
		"app/Modules/*/Database/Migrations",
		"app/Modules/*/Database/migrations",
		"Modules/*/Database/Migrations",
		"Modules/*/Database/migrations",
		"custom/migrations",
	}
	if !reflect.DeepEqual(got.MigrationDirs, wantDirs) {
		t.Fatalf("MigrationDirs mismatch:\n got=%v\nwant=%v", got.MigrationDirs, wantDirs)
	}
	if !reflect.DeepEqual(got.FileGlobs, []string{"*.php"}) {
		t.Fatalf("FileGlobs duplicate not collapsed: %v", got.FileGlobs)
	}
	if !reflect.DeepEqual(got.Lockfiles, []string{"composer.lock", "extra.lock"}) {
		t.Fatalf("Lockfiles mismatch: %v", got.Lockfiles)
	}
}

func TestSpecFromYAML_UserHashModeOverridesFrameworkDefault(t *testing.T) {
	got := specFromYAML(config.MigrationSpec{
		Framework: "laravel", // default HashFilename
		HashMode:  "checksum",
	})
	if got.HashMode != framework.HashChecksum {
		t.Fatalf("user HashMode lost: got %q", got.HashMode)
	}
}

func TestSpecFromYAML_UnknownFrameworkFallsBackToGlobalDefaults(t *testing.T) {
	got := specFromYAML(config.MigrationSpec{
		Framework:     "homegrown",
		MigrationDirs: []string{"db"},
		FileGlobs:     []string{"*.sql"},
	})
	if got.HashMode != framework.HashFilename {
		t.Fatalf("expected fallback HashFilename, got %q", got.HashMode)
	}
	if got.OnModify != framework.OnRebuild {
		t.Fatalf("expected fallback OnRebuild, got %q", got.OnModify)
	}
	if !reflect.DeepEqual(got.MigrationDirs, []string{"db"}) {
		t.Fatalf("unknown framework should not inject built-in dirs: %v", got.MigrationDirs)
	}
	if !reflect.DeepEqual(got.FileGlobs, []string{"*.sql"}) {
		t.Fatalf("FileGlobs mismatch: %v", got.FileGlobs)
	}
	if got.Lockfiles != nil {
		t.Fatalf("expected nil Lockfiles for unknown framework, got %v", got.Lockfiles)
	}
}

func TestMergeStringSetsEmptyInputs(t *testing.T) {
	if got := mergeStringSets(nil, nil); got != nil {
		t.Fatalf("expected nil for empty inputs, got %v", got)
	}
}
