package gsync

import "testing"

func TestNeedsExecFetch(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		// Azure DevOps, both current and legacy hostnames.
		{"ebillet@vs-ssh.visualstudio.com:v3/ebillet/linuxaid-config/linuxaid-config", true},
		{"ssh://git@ssh.dev.azure.com/v3/org/project/repo", true},
		{"git@ssh.dev.azure.com:v3/org/project/repo", true},
		{"ssh://org@myorg.visualstudio.com/DefaultCollection/_git/repo", true},

		// Everything else keeps using go-git.
		{"ssh://git@gitea.obmondo.com:2223/EnableIT/clojure-api.git", false},
		{"git@github.com:obmondo/gfetch.git", false},
		{"ssh://git@gitea-staging-ssh.monitoring-staging.svc.cluster.local:2222/org/repo.git", false},
		{"https://github.com/git/git.git", false},

		// A host merely containing the string must not match.
		{"git@notvisualstudio.com.example.org:org/repo.git", false},
	}

	for _, c := range cases {
		if got := needsExecFetch(c.url); got != c.want {
			t.Errorf("needsExecFetch(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}
