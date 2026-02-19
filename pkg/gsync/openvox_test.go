package gsync

import (
	"testing"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"production", "production"},
		{"feature-branch", "feature_branch"},
		{"v1.0.0", "v1_0_0"},
		{"feature-auth", "feature_auth"},
		{"v2.0.0", "v2_0_0"},
		{"a-b.c", "a_b_c"},
		{"no_change", "no_change"},
		{"", ""},
		{"---", "___"},
		{"...", "___"},
		{"a-b-c.d.e", "a_b_c_d_e"},
		{"feature/my-branch", "feature_my_branch"},
		{"bugfix/auth/login", "bugfix_auth_login"},
		{"user@domain", "user_domain"},
		{"release/v1.0.0-rc1", "release_v1_0_0_rc1"},
		{"branch~1", "branch_1"},
		{"branch^2", "branch_2"},
		{"my branch", "my_branch"},
		{"a//b", "a__b"},
	}

	for _, tt := range tests {
		got := SanitizeName(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDetectCollisions(t *testing.T) {
	t.Run("no collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{"main", "develop", "feature-auth"}
		if msg := detectCollisions(names, m); msg != "" {
			t.Errorf("expected no collision, got: %s", msg)
		}
	})

	t.Run("hyphen vs dot collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{"a-b", "a.b"}
		msg := detectCollisions(names, m)
		if msg == "" {
			t.Error("expected collision between a-b and a.b")
		}
	})

	t.Run("collision across calls", func(t *testing.T) {
		m := make(map[string]string)
		// First call with branches.
		if msg := detectCollisions([]string{"feature-1"}, m); msg != "" {
			t.Errorf("unexpected collision: %s", msg)
		}
		// Second call with tags that collides.
		msg := detectCollisions([]string{"feature.1"}, m)
		if msg == "" {
			t.Error("expected collision between feature-1 (branch) and feature.1 (tag)")
		}
	})

	t.Run("slash vs hyphen collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{"feature/auth", "feature-auth"}
		msg := detectCollisions(names, m)
		if msg == "" {
			t.Error("expected collision between feature/auth and feature-auth")
		}
	})

	t.Run("same name no collision", func(t *testing.T) {
		m := make(map[string]string)
		names := []string{"main", "main"}
		if msg := detectCollisions(names, m); msg != "" {
			t.Errorf("same name should not collide, got: %s", msg)
		}
	})
}
