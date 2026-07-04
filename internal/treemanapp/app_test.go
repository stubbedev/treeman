package treemanapp

import "testing"

// TestCommandTree guards the root command declaration: the commands
// users (and gen-docs) depend on exist, names + aliases are unique,
// and every command carries a name. A vanished or duplicated command
// would otherwise only surface when someone runs the binary.
func TestCommandTree(t *testing.T) {
	root := New()
	if root.Name != "treeman" || root.Version == "" {
		t.Fatalf("root = %q version=%q", root.Name, root.Version)
	}
	seen := map[string]string{}
	for _, c := range root.Commands {
		if c.Name == "" {
			t.Error("command with empty name")
		}
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if prev, dup := seen[n]; dup {
				t.Errorf("name/alias %q claimed by both %q and %q", n, prev, c.Name)
			}
			seen[n] = c.Name
		}
	}
	for _, want := range []string{
		"worktree", "git", "status", "prepare", "logs", "config", "schema", "daemon",
		"init", "doctor", "registry", "snapshots", "mcp",
	} {
		if _, ok := seen[want]; !ok {
			t.Errorf("expected command/alias %q missing from root tree", want)
		}
	}
	// Sub-surfaces added recently — keep them pinned.
	daemon := seen["daemon"]
	for _, c := range root.Commands {
		if c.Name != daemon {
			continue
		}
		subs := map[string]bool{}
		for _, s := range c.Commands {
			subs[s.Name] = true
		}
		for _, want := range []string{"start", "stop", "reload", "status", "state", "install", "uninstall"} {
			if !subs[want] {
				t.Errorf("daemon subcommand %q missing", want)
			}
		}
	}
}
