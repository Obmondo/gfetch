package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const branchMain = "main"

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

	wildcard := Pattern{Raw: "*"}
	if !wildcard.Matches("anything") {
		t.Error("wildcard should match any string")
	}
	if !wildcard.Matches(branchMain) {
		t.Error("wildcard should match main")
	}
	if !wildcard.Matches("v1.0.0") {
		t.Error("wildcard should match v1.0.0")
	}
}

func TestPattern_MatchesBranches(t *testing.T) {
	literal := Pattern{Raw: branchMain}
	if !literal.Matches(branchMain) {
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
  test-repo:
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
	r, ok := cfg.Repos["test-repo"]
	if !ok {
		t.Fatal("repo test-repo not found in map")
	}
	if r.Name != "test-repo" {
		t.Errorf("name = %q, want %q", r.Name, "test-repo")
	}
	if time.Duration(r.PollInterval) != time.Minute {
		t.Errorf("poll_interval = %v, want %v", time.Duration(r.PollInterval), time.Minute)
	}
}

func TestLoad_NameValidation(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name: "valid name",
			content: `repos:
  valid.name-123_abc:
    url: git@github.com:test/repo.git
    local_path: /tmp/repo
    branches: [main]
    ssh_key_path: /tmp/key`,
			wantErr: false,
		},
		{
			name: "too long name",
			content: `repos:
  this-name-is-definitely-longer-than-sixty-four-characters-which-should-be-rejected:
    url: git@github.com:test/repo.git
    branches: [main]`,
			wantErr: true,
		},
		{
			name: "invalid characters",
			content: `repos:
  "invalid name":
    url: git@github.com:test/repo.git
    branches: [main]`,
			wantErr: true,
		},
		{
			name: "list not allowed",
			content: `repos:
  - name: repo1
    url: git@github.com:test/repo.git`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(cfgPath, []byte(tt.content), 0644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(cfgPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_PollIntervalTooLow(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Repos: map[string]RepoConfig{"test": {
		RepoDefaults: RepoDefaults{
			SSHKeyPath:   keyFile,
			LocalPath:    "/tmp/test",
			PollInterval: Duration(5 * time.Second),
			Branches:     []Pattern{{Raw: branchMain}},
		},
		Name: "test",
		URL:  "git@github.com:test/repo.git",
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for low poll interval")
	}
}

func TestValidate_HTTPSPublicRepo(t *testing.T) {
	cfg := &Config{Repos: map[string]RepoConfig{"public-repo": {
		RepoDefaults: RepoDefaults{
			LocalPath:    "/tmp/test",
			PollInterval: Duration(30 * time.Second),
			Branches:     []Pattern{{Raw: branchMain}},
		},
		Name: "public-repo",
		URL:  "https://github.com/git/git.git",
	}}}
	err := cfg.Validate()
	if err != nil {
		t.Errorf("expected HTTPS public repo to pass validation, got: %v", err)
	}
}

func TestLoad_Directory(t *testing.T) {
	dir := t.TempDir()

	repo1Dir := filepath.Join(dir, "repo1")
	if err := os.MkdirAll(repo1Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo1Dir, "config.yaml"), []byte(`repos:
  repo1:
    url: git@github.com:test/repo1.git
    ssh_key_path: /tmp/key1
    local_path: /tmp/repo1
    poll_interval: 1m
    branches:
      - main
`), 0644); err != nil {
		t.Fatal(err)
	}

	repo2Dir := filepath.Join(dir, "repo2")
	if err := os.MkdirAll(repo2Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo2Dir, "config.yaml"), []byte(`repos:
  repo2:
    url: git@github.com:test/repo2.git
    ssh_key_path: /tmp/key2
    local_path: /tmp/repo2
    poll_interval: 2m
    branches:
      - develop
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(cfg.Repos))
	}
	if _, ok := cfg.Repos["repo1"]; !ok {
		t.Error("repo1 not found")
	}
	if _, ok := cfg.Repos["repo2"]; !ok {
		t.Error("repo2 not found")
	}
}

func TestLoad_DirectoryWithGlobal(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "global.yaml"), []byte(`ssh_key_path: /tmp/global_key
local_path: /tmp/global_path
poll_interval: 5m
branches:
  - "*"
`), 0644); err != nil {
		t.Fatal(err)
	}

	repo1Dir := filepath.Join(dir, "repo1")
	if err := os.MkdirAll(repo1Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo1Dir, "config.yaml"), []byte(`repos:
  repo1:
    url: git@github.com:test/repo1.git
    ssh_key_path: /tmp/override_key
    branches:
      - main
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	r1, ok := cfg.Repos["repo1"]
	if !ok {
		t.Fatal("repo1 not found")
	}
	if r1.SSHKeyPath != "/tmp/override_key" {
		t.Errorf("repo1 ssh_key_path = %q, want /tmp/override_key", r1.SSHKeyPath)
	}
	if r1.LocalPath != "/tmp/global_path" {
		t.Errorf("repo1 local_path = %q, want /tmp/global_path", r1.LocalPath)
	}
}

func TestApplyDefaults(t *testing.T) {
	openvoxTrue := true
	defaults := &RepoDefaults{
		SSHKeyPath:   "/tmp/default_key",
		LocalPath:    "/tmp/default_path",
		PollInterval: Duration(3 * time.Minute),
		Branches:     []Pattern{{Raw: "main"}},
		OpenVox:      &openvoxTrue,
	}

	repo := &RepoConfig{
		Name: "test",
		URL:  "git@github.com:test/repo.git",
	}
	applyDefaults(repo, defaults)

	if repo.SSHKeyPath != "/tmp/default_key" {
		t.Errorf("ssh_key_path = %q, want /tmp/default_key", repo.SSHKeyPath)
	}
	if repo.OpenVox == nil || !*repo.OpenVox {
		t.Error("openvox should be inherited from defaults")
	}
}

func TestApplyDefaults_PruneBoolOverride(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name           string
		defaultPrune   *bool
		repoPrune      *bool
		wantPrune      *bool
		defaultStale   *bool
		repoStale      *bool
		wantStale      *bool
	}{
		{
			name:         "nil repo inherits default prune=true",
			defaultPrune: boolPtr(true),
			repoPrune:    nil,
			wantPrune:    boolPtr(true),
			defaultStale: boolPtr(true),
			repoStale:    nil,
			wantStale:    boolPtr(true),
		},
		{
			name:         "explicit prune=false blocks default prune=true",
			defaultPrune: boolPtr(true),
			repoPrune:    boolPtr(false),
			wantPrune:    boolPtr(false),
			defaultStale: boolPtr(true),
			repoStale:    boolPtr(false),
			wantStale:    boolPtr(false),
		},
		{
			name:         "nil defaults leave repo nil",
			defaultPrune: nil,
			repoPrune:    nil,
			wantPrune:    nil,
			defaultStale: nil,
			repoStale:    nil,
			wantStale:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defaults := &RepoDefaults{
				SSHKeyPath:   "/tmp/key",
				LocalPath:    "/tmp/path",
				PollInterval: Duration(10 * time.Second),
				Branches:     []Pattern{{Raw: "main"}},
				Prune:        tt.defaultPrune,
				PruneStale:   tt.defaultStale,
			}
			repo := &RepoConfig{
				Name: "test",
				URL:  "git@github.com:test/repo.git",
				RepoDefaults: RepoDefaults{
					Prune:      tt.repoPrune,
					PruneStale: tt.repoStale,
				},
			}
			applyDefaults(repo, defaults)

			switch {
			case tt.wantPrune == nil && repo.Prune != nil:
				t.Errorf("prune: want nil, got %v", *repo.Prune)
			case tt.wantPrune != nil && repo.Prune == nil:
				t.Errorf("prune: want %v, got nil", *tt.wantPrune)
			case tt.wantPrune != nil && *repo.Prune != *tt.wantPrune:
				t.Errorf("prune: want %v, got %v", *tt.wantPrune, *repo.Prune)
			}

			switch {
			case tt.wantStale == nil && repo.PruneStale != nil:
				t.Errorf("prune_stale: want nil, got %v", *repo.PruneStale)
			case tt.wantStale != nil && repo.PruneStale == nil:
				t.Errorf("prune_stale: want %v, got nil", *tt.wantStale)
			case tt.wantStale != nil && *repo.PruneStale != *tt.wantStale:
				t.Errorf("prune_stale: want %v, got %v", *tt.wantStale, *repo.PruneStale)
			}
		})
	}
}
