package shellenv

import "testing"

func TestMergePaths(t *testing.T) {
	cases := []struct {
		name, base, extra, want string
	}{
		{"empty extra returns base", "/a:/b", "", "/a:/b"},
		{"empty base returns extra", "", "/x:/y", "/x:/y"},
		{"both empty", "", "", ""},
		{"appends only new dirs", "/a:/b", "/b:/c", "/a:/b:/c"},
		{"base precedence preserved", "/user/bin", "/nix/bin:/user/bin", "/user/bin:/nix/bin"},
		{"all new appended in order", "/a", "/b:/c", "/a:/b:/c"},
		{"skips empty segments in extra", "/a", ":/b:", "/a:/b"},
		{"fully overlapping extra is no-op", "/a:/b", "/a:/b", "/a:/b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MergePaths(c.base, c.extra); got != c.want {
				t.Errorf("MergePaths(%q, %q) = %q, want %q", c.base, c.extra, got, c.want)
			}
		})
	}
}

// TestBaseEnvMergesLoginPath asserts the inheritedEnv PATH keeps
// precedence (leads) while the login-shell PATH is folded in after.
func TestBaseEnvMergesLoginPath(t *testing.T) {
	orig := LoginShellPATH
	defer func() { LoginShellPATH = orig }()
	LoginShellPATH = func() string { return "/nix-profile/bin:/user/shims" }

	env := BaseEnv(map[string]string{"PATH": "/user/shims:/user/bin"})
	got := env["PATH"]
	const wantPrefix = "/user/shims:/user/bin"
	if got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("inheritedEnv PATH should lead, got %q", got)
	}
	if got != "/user/shims:/user/bin:/nix-profile/bin" {
		t.Errorf("login-shell dirs not merged correctly: %q", got)
	}
}
