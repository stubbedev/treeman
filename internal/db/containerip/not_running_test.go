package containerip

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptedEngine installs a fake container engine CLI on PATH whose
// subcommands run the given shell snippets; any other one fails.
func scriptedEngine(t *testing.T, cases map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	name := "tm-scripted-docker-" + strings.NewReplacer("/", "_", " ", "_", "-", "_").Replace(strings.TrimPrefix(t.Name(), "Test"))
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncase \"$1\" in\n  info) echo ok;;\n")
	for sub, body := range cases {
		script.WriteString("  " + sub + ") " + body + ";;\n")
	}
	script.WriteString("  *) exit 1;;\nesac\n")
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return name
}

// Regression for #212: a stopped or not-yet-created container is a
// typed *NotRunningError, so the create retry and the startup wait can
// treat it like an engine that is still starting; an engine CLI that
// fails for another reason is not.
func TestResolveAddrReportsContainersThatAreNotRunning(t *testing.T) {
	cases := []struct {
		name        string
		opts        Opts
		engine      map[string]string
		wantRunning bool
		wantCreated bool
	}{
		{
			name:        "stopped container",
			opts:        Opts{Container: "app-mysql-1", InternalPort: 3306},
			engine:      map[string]string{"inspect": `echo '[{"State": {"Running": false}}]'`},
			wantCreated: true,
		},
		{
			name:   "container not created",
			opts:   Opts{Container: "app-mysql-1", InternalPort: 3306},
			engine: map[string]string{"inspect": "exit 1", "ps": "exit 0"},
		},
		{
			name:   "compose service with no running container",
			opts:   Opts{ComposeService: "mysql", ComposeProject: "app", InternalPort: 3306},
			engine: map[string]string{"ps": "exit 0"},
		},
		{
			name:        "engine CLI failing",
			opts:        Opts{Container: "app-mysql-1", InternalPort: 3306},
			engine:      map[string]string{"inspect": "exit 1", "ps": "exit 1"},
			wantRunning: true,
		},
		{
			name:        "inspect failing for a container that exists",
			opts:        Opts{Container: "app-mysql-1", InternalPort: 3306},
			engine:      map[string]string{"inspect": "exit 1", "ps": "echo 0123abcd"},
			wantRunning: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Engine = scriptedEngine(t, tc.engine)
			_, err := ResolveAddr(context.Background(), tc.opts)
			if err == nil {
				t.Fatal("ResolveAddr succeeded, want a failure")
			}
			nr, isNotRunning := errors.AsType[*NotRunningError](err)
			if isNotRunning == tc.wantRunning {
				t.Fatalf("err = %v; not-running = %v, want %v", err, isNotRunning, !tc.wantRunning)
			}
			if isNotRunning && nr.Created != tc.wantCreated {
				t.Errorf("Created = %v, want %v", nr.Created, tc.wantCreated)
			}
		})
	}
}
