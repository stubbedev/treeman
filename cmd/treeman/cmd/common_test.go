package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestAbsPathFailsWhenTheWorkingDirectoryIsUnknown(t *testing.T) {
	gone := errors.New("getwd: no such file or directory")
	got, err := absPathFrom(func() (string, error) { return "", gone }, "some-worktree")
	if err == nil {
		t.Fatalf("absPathFrom with a failing getwd = %q, nil; want an error instead of the unresolved path", got)
	}
	if !errors.Is(err, gone) || !strings.Contains(err.Error(), "some-worktree") {
		t.Errorf("absPathFrom error must wrap the getwd failure and name the input: %v", err)
	}
}

func TestAbsPathResolvesAgainstTheWorkingDirectory(t *testing.T) {
	cwd := filepath.FromSlash("/home/dev/repo")
	getwd := func() (string, error) { return cwd, nil }
	cases := map[string]string{
		"../sibling":                        filepath.FromSlash("/home/dev/sibling"),
		"wt":                                filepath.FromSlash("/home/dev/repo/wt"),
		filepath.FromSlash("/abs/./x/../y"): filepath.FromSlash("/abs/y"),
	}
	for in, want := range cases {
		got, err := absPathFrom(getwd, in)
		if err != nil || got != want {
			t.Errorf("absPathFrom(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
