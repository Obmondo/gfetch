package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPattern_IsRegex(t *testing.T) {
	tests := []struct {
		raw    string
		expect bool
	}{
		{"v1.0.0", false},
		{"/^v[0-9]+\\./", true},
		{"/", false},
		{"//", false},
		{"/abc/", true},
	}
	for _, tt := range tests {
		p := Pattern{Raw: tt.raw}
		if got := p.IsRegex(); got != tt.expect {
			t.Errorf("Pattern{%q}.IsRegex() = %v, want %v", tt.raw, got, tt.expect)
		}
	}
}

func TestPattern_Matches(t *testing.T) {
	literal := Pattern{Raw: "v1.0.0"}
	if !literal.Matches("v1.0.0") {
		t.Error("literal should match exact string")
	}
	if literal.Matches("v1.0.1") {
		t.Error("literal should not match different string")
	}

	regex := Pattern{Raw: "/^v[0-9]+\\./"}
	if err := regex.Compile(); err != nil {
		t.Fatal(err)
	}
	if !regex.Matches("v1.2.3") {
		t.Error("regex should match v1.2.3")
	}
	if regex.Matches("release-1.0") {
		t.Error("regex should not match release-1.0")
	}
}

func TestPattern_MatchesBranches(t *testing.T) {
	literal := Pattern{Raw: "main"}
	if !literal.Matches("main") {
		t.Error("literal should match exact branch name")
	}
	if literal.Matches("main2") {
		t.Error("literal should not match different branch name")
	}

	regex := Pattern{Raw: "/^release-.*/"}
	if err := regex.Compile(); err != nil {
		t.Fatal(err)
	}
	if !regex.Matches("release-1.0") {
		t.Error("regex should match release-1.0")
	}
	if !regex.Matches("release-2.0-beta") {
		t.Error("regex should match release-2.0-beta")
	}
	if regex.Matches("main") {
		t.Error("regex should not match main")
	}
}

func TestLoad(t *testing.T) {
	content := `repos:
  - name: test-repo
    url: git@github.com:test/repo.git
    ssh_key_path: /tmp/test_key
    local_path: /tmp/test_repo
    poll_interval: 1m
    branches:
      - main
      - /^release-.*/
    tags:
      - v1.0.0
      - /^v[0-9]+\./
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(cfg.Repos))
	}
	r := cfg.Repos[0]
	if r.Name != "test-repo" {
		t.Errorf("name = %q, want %q", r.Name, "test-repo")
	}
	if r.PollInterval != time.Minute {
		t.Errorf("poll_interval = %v, want %v", r.PollInterval, time.Minute)
	}
	if len(r.Branches) != 2 {
		t.Fatalf("branches count = %d, want 2", len(r.Branches))
	}
	if r.Branches[0].Raw != "main" {
		t.Errorf("branches[0] = %q, want %q", r.Branches[0].Raw, "main")
	}
	if r.Branches[1].Raw != "/^release-.*/" {
		t.Errorf("branches[1] = %q, want %q", r.Branches[1].Raw, "/^release-.*/")
	}
	if len(r.Tags) != 2 {
		t.Errorf("tags count = %d, want 2", len(r.Tags))
	}
}

func TestValidate_MissingFields(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty config")
	}

	cfg = &Config{Repos: []RepoConfig{{Name: ""}}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing name")
	}
}

func TestValidate_PollIntervalTooLow(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{{
		Name:         "test",
		URL:          "git@github.com:test/repo.git",
		SSHKeyPath:   keyFile,
		LocalPath:    "/tmp/test",
		PollInterval: 5 * time.Second,
		Branches:     []Pattern{{Raw: "main"}},
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for low poll interval")
	}
}

func TestValidate_DuplicateNames(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{
		{Name: "dup", URL: "git@a.git", SSHKeyPath: keyFile, LocalPath: "/tmp/a", PollInterval: 30 * time.Second, Branches: []Pattern{{Raw: "main"}}},
		{Name: "dup", URL: "git@b.git", SSHKeyPath: keyFile, LocalPath: "/tmp/b", PollInterval: 30 * time.Second, Branches: []Pattern{{Raw: "main"}}},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for duplicate names")
	}
}

func TestValidate_HTTPSPublicRepo(t *testing.T) {
	cfg := &Config{Repos: []RepoConfig{{
		Name:         "public-repo",
		URL:          "https://github.com/git/git.git",
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Branches:     []Pattern{{Raw: "main"}},
	}}}
	err := cfg.Validate()
	if err != nil {
		t.Errorf("expected HTTPS public repo to pass validation, got: %v", err)
	}
}

func TestValidate_SSHRepoRequiresKey(t *testing.T) {
	cfg := &Config{Repos: []RepoConfig{{
		Name:         "ssh-repo",
		URL:          "git@github.com:test/repo.git",
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Branches:     []Pattern{{Raw: "main"}},
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for SSH URL without ssh_key_path")
	}
}

func TestValidate_InvalidTagRegex(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{{
		Name:         "test",
		URL:          "git@github.com:test/repo.git",
		SSHKeyPath:   keyFile,
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Tags:         []Pattern{{Raw: "/[invalid/"}},
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for invalid tag regex")
	}
}

func TestValidate_CheckoutMatchesLiteralBranch(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{{
		Name:         "test",
		URL:          "git@github.com:test/repo.git",
		SSHKeyPath:   keyFile,
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Branches:     []Pattern{{Raw: "main"}, {Raw: "develop"}},
		Checkout:     "main",
	}}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected checkout=main to pass validation, got: %v", err)
	}
}

func TestValidate_CheckoutMatchesRegexBranch(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{{
		Name:         "test",
		URL:          "git@github.com:test/repo.git",
		SSHKeyPath:   keyFile,
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Branches:     []Pattern{{Raw: "/^release-.*/"}},
		Checkout:     "release-1.0",
	}}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected checkout=release-1.0 to match regex pattern, got: %v", err)
	}
}

func TestValidate_CheckoutMatchesTag(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{{
		Name:         "test",
		URL:          "git@github.com:test/repo.git",
		SSHKeyPath:   keyFile,
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Branches:     []Pattern{{Raw: "main"}},
		Tags:         []Pattern{{Raw: "v1.0.0"}},
		Checkout:     "v1.0.0",
	}}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected checkout=v1.0.0 to match tag pattern, got: %v", err)
	}
}

func TestValidate_CheckoutNoMatch(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{{
		Name:         "test",
		URL:          "git@github.com:test/repo.git",
		SSHKeyPath:   keyFile,
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Branches:     []Pattern{{Raw: "main"}},
		Checkout:     "nonexistent",
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for checkout that doesn't match any branch or tag")
	}
}

func TestValidate_CheckoutEmpty(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{{
		Name:         "test",
		URL:          "git@github.com:test/repo.git",
		SSHKeyPath:   keyFile,
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Branches:     []Pattern{{Raw: "main"}},
		Checkout:     "",
	}}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected empty checkout to pass validation, got: %v", err)
	}
}

func TestLoad_WithCheckout(t *testing.T) {
	content := `repos:
  - name: test-repo
    url: git@github.com:test/repo.git
    ssh_key_path: /tmp/test_key
    local_path: /tmp/test_repo
    poll_interval: 1m
    checkout: main
    branches:
      - main
      - develop
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Repos[0].Checkout != "main" {
		t.Errorf("checkout = %q, want %q", cfg.Repos[0].Checkout, "main")
	}
}

func TestValidate_InvalidBranchRegex(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	os.WriteFile(keyFile, []byte("fake"), 0600)

	cfg := &Config{Repos: []RepoConfig{{
		Name:         "test",
		URL:          "git@github.com:test/repo.git",
		SSHKeyPath:   keyFile,
		LocalPath:    "/tmp/test",
		PollInterval: 30 * time.Second,
		Branches:     []Pattern{{Raw: "/[invalid/"}},
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for invalid branch regex")
	}
}

func TestLoad_Directory(t *testing.T) {
	dir := t.TempDir()

	// Create repo1/config.yaml
	repo1Dir := filepath.Join(dir, "repo1")
	os.MkdirAll(repo1Dir, 0755)
	os.WriteFile(filepath.Join(repo1Dir, "config.yaml"), []byte(`repos:
  - name: repo1
    url: git@github.com:test/repo1.git
    ssh_key_path: /tmp/key1
    local_path: /tmp/repo1
    poll_interval: 1m
    branches:
      - main
`), 0644)

	// Create repo2/config.yaml
	repo2Dir := filepath.Join(dir, "repo2")
	os.MkdirAll(repo2Dir, 0755)
	os.WriteFile(filepath.Join(repo2Dir, "config.yaml"), []byte(`repos:
  - name: repo2
    url: git@github.com:test/repo2.git
    ssh_key_path: /tmp/key2
    local_path: /tmp/repo2
    poll_interval: 2m
    branches:
      - develop
`), 0644)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(cfg.Repos))
	}

	// repos are loaded in glob order (alphabetical by directory name)
	names := map[string]bool{}
	for _, r := range cfg.Repos {
		names[r.Name] = true
	}
	if !names["repo1"] || !names["repo2"] {
		t.Errorf("expected repos repo1 and repo2, got %v", names)
	}
}

func TestLoad_DirectoryWithGlobal(t *testing.T) {
	dir := t.TempDir()

	// Create global.yaml with defaults
	os.WriteFile(filepath.Join(dir, "global.yaml"), []byte(`ssh_key_path: /tmp/global_key
ssh_known_hosts: |
  example.com ssh-ed25519 AAAA...
local_path: /tmp/global_path
poll_interval: 5m
`), 0644)

	// Create repo1/config.yaml — overrides ssh_key_path, inherits the rest
	repo1Dir := filepath.Join(dir, "repo1")
	os.MkdirAll(repo1Dir, 0755)
	os.WriteFile(filepath.Join(repo1Dir, "config.yaml"), []byte(`repos:
  - name: repo1
    url: git@github.com:test/repo1.git
    ssh_key_path: /tmp/override_key
    branches:
      - main
`), 0644)

	// Create repo2/config.yaml — inherits everything from global
	repo2Dir := filepath.Join(dir, "repo2")
	os.MkdirAll(repo2Dir, 0755)
	os.WriteFile(filepath.Join(repo2Dir, "config.yaml"), []byte(`repos:
  - name: repo2
    url: git@github.com:test/repo2.git
    branches:
      - develop
`), 0644)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(cfg.Repos))
	}

	// Find repos by name
	repos := map[string]*RepoConfig{}
	for i := range cfg.Repos {
		repos[cfg.Repos[i].Name] = &cfg.Repos[i]
	}

	// repo1 should have overridden ssh_key_path but inherited the rest
	r1 := repos["repo1"]
	if r1.SSHKeyPath != "/tmp/override_key" {
		t.Errorf("repo1 ssh_key_path = %q, want /tmp/override_key", r1.SSHKeyPath)
	}
	if r1.LocalPath != "/tmp/global_path" {
		t.Errorf("repo1 local_path = %q, want /tmp/global_path", r1.LocalPath)
	}
	if r1.PollInterval != 5*time.Minute {
		t.Errorf("repo1 poll_interval = %v, want 5m", r1.PollInterval)
	}
	if r1.SSHKnownHosts == "" {
		t.Error("repo1 ssh_known_hosts should be inherited from global")
	}

	// repo2 should inherit everything
	r2 := repos["repo2"]
	if r2.SSHKeyPath != "/tmp/global_key" {
		t.Errorf("repo2 ssh_key_path = %q, want /tmp/global_key", r2.SSHKeyPath)
	}
	if r2.LocalPath != "/tmp/global_path" {
		t.Errorf("repo2 local_path = %q, want /tmp/global_path", r2.LocalPath)
	}
	if r2.PollInterval != 5*time.Minute {
		t.Errorf("repo2 poll_interval = %v, want 5m", r2.PollInterval)
	}
}

func TestApplyDefaults(t *testing.T) {
	defaults := &RepoDefaults{
		SSHKeyPath:    "/tmp/default_key",
		SSHKnownHosts: "example.com ssh-ed25519 AAAA...",
		LocalPath:     "/tmp/default_path",
		PollInterval:  3 * time.Minute,
	}

	// Repo with all fields empty — should inherit all defaults
	repo := &RepoConfig{
		Name:     "test",
		URL:      "git@github.com:test/repo.git",
		Branches: []Pattern{{Raw: "main"}},
	}
	applyDefaults(repo, defaults)

	if repo.SSHKeyPath != "/tmp/default_key" {
		t.Errorf("ssh_key_path = %q, want /tmp/default_key", repo.SSHKeyPath)
	}
	if repo.SSHKnownHosts != "example.com ssh-ed25519 AAAA..." {
		t.Errorf("ssh_known_hosts = %q, want default", repo.SSHKnownHosts)
	}
	if repo.LocalPath != "/tmp/default_path" {
		t.Errorf("local_path = %q, want /tmp/default_path", repo.LocalPath)
	}
	if repo.PollInterval != 3*time.Minute {
		t.Errorf("poll_interval = %v, want 3m", repo.PollInterval)
	}

	// Repo with fields set — should NOT be overridden
	repo2 := &RepoConfig{
		Name:          "test2",
		URL:           "git@github.com:test/repo2.git",
		SSHKeyPath:    "/tmp/my_key",
		SSHKnownHosts: "custom.com ssh-rsa BBBB...",
		LocalPath:     "/tmp/my_path",
		PollInterval:  1 * time.Minute,
		Branches:      []Pattern{{Raw: "main"}},
	}
	applyDefaults(repo2, defaults)

	if repo2.SSHKeyPath != "/tmp/my_key" {
		t.Errorf("ssh_key_path should not be overridden, got %q", repo2.SSHKeyPath)
	}
	if repo2.SSHKnownHosts != "custom.com ssh-rsa BBBB..." {
		t.Errorf("ssh_known_hosts should not be overridden, got %q", repo2.SSHKnownHosts)
	}
	if repo2.LocalPath != "/tmp/my_path" {
		t.Errorf("local_path should not be overridden, got %q", repo2.LocalPath)
	}
	if repo2.PollInterval != 1*time.Minute {
		t.Errorf("poll_interval should not be overridden, got %v", repo2.PollInterval)
	}
}

func TestLoad_FileWithTopLevelSSHKnownHosts(t *testing.T) {
	content := `ssh_known_hosts: |
  example.com ssh-ed25519 AAAA...

repos:
  - name: test-repo
    url: git@github.com:test/repo.git
    ssh_key_path: /tmp/test_key
    local_path: /tmp/test_repo
    poll_interval: 1m
    branches:
      - main
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Repos[0].SSHKnownHosts == "" {
		t.Error("expected top-level ssh_known_hosts to be applied to repo")
	}
}
