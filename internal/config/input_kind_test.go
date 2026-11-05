package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInputKindParsesAtTheBoundary(t *testing.T) {
	var inputs []Input
	body := `
- database/migrations/**/*.php
- {glob: "app/*/migrations/*.php", kind: migrations}
- {glob: composer.lock, label: lockfile, kind: lockfile}
`
	if err := yaml.Unmarshal([]byte(body), &inputs); err != nil {
		t.Fatal(err)
	}
	want := []InputKind{InputKindMigrations, InputKindMigrations, InputKindLockfile}
	for i, in := range inputs {
		if in.Kind != want[i] {
			t.Errorf("inputs[%d].Kind = %q, want %q", i, in.Kind, want[i])
		}
	}
	got := LockfileGlobs(inputs)
	if _, ok := got["composer.lock"]; !ok || len(got) != 1 {
		t.Errorf("LockfileGlobs = %v, want only composer.lock", got)
	}
}

func TestInputKindRejectsUnknownValue(t *testing.T) {
	var inputs []Input
	err := yaml.Unmarshal([]byte(`- {glob: composer.lock, kind: lockfiles}`), &inputs)
	if err == nil || !strings.Contains(err.Error(), `"lockfiles"`) {
		t.Fatalf("a mistyped kind must fail the load naming the value, got %v", err)
	}
}
