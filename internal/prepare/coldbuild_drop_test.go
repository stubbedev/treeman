package prepare

import "testing"

func TestNameOwnedByOtherSlug(t *testing.T) {
	others := []string{"kon_12463", "main_master", "a"}
	cases := []struct {
		name string
		want bool
	}{
		// Owned by another slug — boundary on `_`.
		{"kontainer_testing_kon_12463", true},
		{"kontainer_testing_kon_12463_test_1", true},
		{"kho_kon_12463_client_48_category_231_pim_end", true},
		{"kontainer_testing_main_master", true},
		{"kontainer_testing_a", true},
		{"kontainer_testing_a_test_3", true},

		// Not owned — main worktree's own bare names.
		{"kontainer_testing", false},
		{"kontainer_testing_test_1", false},
		{"kho", false},

		// Prefix-collision guard: slug `a` must not match `_abc`.
		{"kontainer_testing_abc", false},
		{"kontainer_testing_abc_test_3", false},
		// Slug `kon_12463` must not match `_kon_12463xy`.
		{"kontainer_testing_kon_12463xy", false},
	}
	for _, c := range cases {
		got := nameOwnedByOtherSlug(c.name, others)
		if got != c.want {
			t.Errorf("nameOwnedByOtherSlug(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNameOwnedByOtherSlugEmptyInputs(t *testing.T) {
	if nameOwnedByOtherSlug("kontainer_testing_kon_12463", nil) {
		t.Errorf("nil otherSlugs should never report owned")
	}
	if nameOwnedByOtherSlug("anything", []string{""}) {
		t.Errorf("empty slug entry must be ignored")
	}
}
