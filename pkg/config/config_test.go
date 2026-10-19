package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"gopkg.in/yaml.v3"

	"github.com/obmondo/gfetch/pkg/telemetry"
)

const (
	branchMain    = "main"
	tagSemverGlob = `/^v[0-9]+\./`
	testRepoName  = "test"
	testLocalPath = "/tmp/test"
	testRepoURL   = "git@github.com:test/repo.git"
	goodRepoName  = "good"
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

	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: {
		RepoDefaults: RepoDefaults{
			SSHKeyPath:   keyFile,
			LocalPath:    testLocalPath,
			PollInterval: Duration(5 * time.Second),
			Branches:     []Pattern{{Raw: branchMain}},
		},
		Name: testRepoName,
		URL:  testRepoURL,
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for low poll interval")
	}
}

func TestValidate_HTTPSPublicRepo(t *testing.T) {
	cfg := &Config{Repos: map[string]RepoConfig{"public-repo": {
		RepoDefaults: RepoDefaults{
			LocalPath:    testLocalPath,
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

// TestLoad_DirectoryDuplicateAcrossSubdirs verifies that a duplicate repo name
// across two per-repo-subdir files is reported as a fatal error.
func TestLoad_DirectoryDuplicateAcrossSubdirs(t *testing.T) {
	dir := t.TempDir()

	body := []byte(`repos:
  shared-name:
    url: git@github.com:test/repo.git
    ssh_key_path: /tmp/key
    local_path: /tmp/repo
    poll_interval: 1m
    branches: [main]
`)
	for _, sub := range []string{"first", "second"} {
		subDir := filepath.Join(dir, sub)
		if err := os.MkdirAll(subDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "config.yaml"), body, 0644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
}

// TestValidate_DropsInvalidRepoAndKeepsRest asserts that one repo with a
// validation error is dropped (with metric increment) while a sibling repo
// still validates successfully.
func TestValidate_DropsInvalidRepoAndKeepsRest(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Repos: map[string]RepoConfig{
		goodRepoName: {
			RepoDefaults: RepoDefaults{
				SSHKeyPath:   keyFile,
				LocalPath:    "/tmp/good",
				PollInterval: Duration(30 * time.Second),
				Branches:     []Pattern{{Raw: branchMain}},
			},
			Name: goodRepoName,
			URL:  "git@github.com:test/good.git",
		},
		"bad-missing-url": {
			RepoDefaults: RepoDefaults{
				SSHKeyPath:   keyFile,
				LocalPath:    "/tmp/bad",
				PollInterval: Duration(30 * time.Second),
				Branches:     []Pattern{{Raw: branchMain}},
			},
			Name: "bad-missing-url",
			// URL intentionally empty
		},
	}}

	before := testutil.ToFloat64(telemetry.ConfigRepoValidateFailuresTotal.WithLabelValues("bad-missing-url"))

	err := cfg.Validate()

	var partial *PartialValidateError
	if !errors.As(err, &partial) {
		t.Fatalf("expected *PartialValidateError, got %T: %v", err, err)
	}
	if _, ok := partial.Failures["bad-missing-url"]; !ok {
		t.Errorf("expected failure for bad-missing-url, got %v", partial.Failures)
	}
	if _, ok := cfg.Repos["bad-missing-url"]; ok {
		t.Error("expected bad-missing-url to be dropped from cfg.Repos")
	}
	if _, ok := cfg.Repos[goodRepoName]; !ok {
		t.Error("expected good repo to remain after partial validate")
	}
	if got := testutil.ToFloat64(telemetry.ConfigRepoValidateFailuresTotal.WithLabelValues("bad-missing-url")); got != before+1 {
		t.Errorf("ConfigRepoValidateFailuresTotal{bad-missing-url}: got %v, want %v", got, before+1)
	}
}

// TestValidate_AllReposInvalid asserts that when every repo fails validation,
// Validate returns *PartialValidateError listing them all and c.Repos is
// emptied. (The reloader and HTTP server treat this as "no usable repos" and
// keep the old config.)
func TestValidate_AllReposInvalid(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Repos: map[string]RepoConfig{
		"bad-a": {
			RepoDefaults: RepoDefaults{SSHKeyPath: keyFile, LocalPath: "/tmp/a", PollInterval: Duration(30 * time.Second), Branches: []Pattern{{Raw: branchMain}}},
			Name:         "bad-a",
		},
		"bad-b": {
			RepoDefaults: RepoDefaults{SSHKeyPath: keyFile, LocalPath: "/tmp/b", PollInterval: Duration(30 * time.Second), Branches: []Pattern{{Raw: branchMain}}},
			Name:         "bad-b",
		},
	}}

	err := cfg.Validate()

	var partial *PartialValidateError
	if !errors.As(err, &partial) {
		t.Fatalf("expected *PartialValidateError, got %T: %v", err, err)
	}
	if len(partial.Failures) != 2 {
		t.Errorf("expected 2 failures, got %d: %v", len(partial.Failures), partial.Failures)
	}
	if len(cfg.Repos) != 0 {
		t.Errorf("expected cfg.Repos to be empty after all-invalid validate, got %v", cfg.Repos)
	}
}

// TestValidate_NoReposConfigured guards the entry condition: an empty Config
// returns the plain "no repos configured" error, not a *PartialValidateError.
// CLI users who provide an empty config still see that explicit message.
func TestValidate_NoReposConfigured(t *testing.T) {
	cfg := &Config{Repos: map[string]RepoConfig{}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty config, got nil")
	}
	if _, ok := errors.AsType[*PartialValidateError](err); ok {
		t.Fatalf("expected plain error, got *PartialValidateError: %v", err)
	}
	if err.Error() != "no repos configured" {
		t.Errorf("expected %q, got %q", "no repos configured", err.Error())
	}
}

func TestApplyDefaults(t *testing.T) {
	openvoxTrue := true
	productionAliasTrue := true
	workers := 8
	defaults := &RepoDefaults{
		SSHKeyPath:        "/tmp/default_key",
		LocalPath:         "/tmp/default_path",
		PollInterval:      Duration(3 * time.Minute),
		Branches:          []Pattern{{Raw: "main"}},
		OpenVox:           &openvoxTrue,
		OpenVoxMaxWorkers: &workers,
		ProductionAlias:   &productionAliasTrue,
	}

	repo := &RepoConfig{
		Name: testRepoName,
		URL:  testRepoURL,
	}
	applyDefaults(repo, defaults)

	if repo.SSHKeyPath != "/tmp/default_key" {
		t.Errorf("ssh_key_path = %q, want /tmp/default_key", repo.SSHKeyPath)
	}
	if repo.OpenVox == nil || !*repo.OpenVox {
		t.Error("openvox should be inherited from defaults")
	}
	if repo.ProductionAlias == nil || !*repo.ProductionAlias {
		t.Error("production_alias should be inherited from defaults")
	}
	if repo.OpenVoxMaxWorkers == nil || *repo.OpenVoxMaxWorkers != workers {
		t.Errorf("openvox_max_workers = %v, want %d", repo.OpenVoxMaxWorkers, workers)
	}
}

func TestValidate_ProductionAliasRequiresOpenVox(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	productionAlias := true
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: {
		RepoDefaults: RepoDefaults{
			SSHKeyPath:      keyFile,
			LocalPath:       testLocalPath,
			PollInterval:    Duration(30 * time.Second),
			Branches:        []Pattern{{Raw: branchMain}},
			ProductionAlias: &productionAlias,
		},
		Name: testRepoName,
		URL:  testRepoURL,
	}}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when production_alias is enabled without openvox")
	}
}

func TestValidate_OpenVoxMaxWorkers(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	openvox := true
	workers := 9
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: {
		RepoDefaults: RepoDefaults{
			SSHKeyPath:        keyFile,
			LocalPath:         testLocalPath,
			PollInterval:      Duration(30 * time.Second),
			Branches:          []Pattern{{Raw: branchMain}},
			OpenVox:           &openvox,
			OpenVoxMaxWorkers: &workers,
		},
		Name: testRepoName,
		URL:  testRepoURL,
	}}}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
}

func TestValidate_OpenVoxMaxWorkersRequiresOpenVox(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	workers := 9
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: {
		RepoDefaults: RepoDefaults{
			SSHKeyPath:        keyFile,
			LocalPath:         testLocalPath,
			PollInterval:      Duration(30 * time.Second),
			Branches:          []Pattern{{Raw: branchMain}},
			OpenVoxMaxWorkers: &workers,
		},
		Name: testRepoName,
		URL:  testRepoURL,
	}}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when openvox_max_workers is set without openvox")
	}
}

func TestValidate_OpenVoxMaxWorkersRange(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	openvox := true
	workers := 0
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: {
		RepoDefaults: RepoDefaults{
			SSHKeyPath:        keyFile,
			LocalPath:         testLocalPath,
			PollInterval:      Duration(30 * time.Second),
			Branches:          []Pattern{{Raw: branchMain}},
			OpenVox:           &openvox,
			OpenVoxMaxWorkers: &workers,
		},
		Name: testRepoName,
		URL:  testRepoURL,
	}}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid openvox_max_workers range")
	}
}

func TestApplyDefaults_PruneBoolOverride(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name         string
		defaultPrune *bool
		repoPrune    *bool
		wantPrune    *bool
		defaultStale *bool
		repoStale    *bool
		wantStale    *bool
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
				Name:       testRepoName,
				URL:        testRepoURL,
				Prune:      tt.repoPrune,
				PruneStale: tt.repoStale,
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

// defaultBranchOnlyRepo builds a repo config that is valid apart from whatever
// the caller adds, so each guard below is tested in isolation.
func defaultBranchOnlyRepo(t *testing.T) RepoConfig {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	defaultBranchOnly := true
	return RepoConfig{
		SSHKeyPath:        keyFile,
		LocalPath:         testLocalPath,
		PollInterval:      Duration(30 * time.Second),
		DefaultBranchOnly: &defaultBranchOnly,
		Name:              testRepoName,
		URL:               testRepoURL,
	}
}

func TestValidate_DefaultBranchOnlyAlone(t *testing.T) {
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: defaultBranchOnlyRepo(t)}}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("default_branch_only on its own should be valid, got %v", err)
	}
}

func TestValidate_DefaultBranchOnlyRejectsBranches(t *testing.T) {
	repo := defaultBranchOnlyRepo(t)
	repo.Branches = []Pattern{{Raw: branchMain}}
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: repo}}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when branches is set alongside default_branch_only")
	}
}

func TestValidate_DefaultBranchOnlyRejectsCheckout(t *testing.T) {
	repo := defaultBranchOnlyRepo(t)
	repo.Checkout = branchMain
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: repo}}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when checkout is set alongside default_branch_only")
	}
}

func TestValidate_DefaultBranchOnlyRejectsOpenVox(t *testing.T) {
	repo := defaultBranchOnlyRepo(t)
	openVox := true
	repo.OpenVox = &openVox
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: repo}}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when default_branch_only is combined with openvox")
	}
}

// default_branch_only means the default branch and nothing else, so tags are
// rejected alongside branches and checkout.
func TestValidate_DefaultBranchOnlyRejectsTags(t *testing.T) {
	repo := defaultBranchOnlyRepo(t)
	repo.Tags = []Pattern{{Raw: tagSemverGlob}}
	cfg := &Config{Repos: map[string]RepoConfig{testRepoName: repo}}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when tags are set alongside default_branch_only")
	}
}

// Inherited branch patterns must not trip the guard: the operator did not set
// branches on this repo, the defaults block did.
//
// This goes through Load rather than Validate on purpose. applyDefaults runs
// only in loadFile/loadDir, so a Validate-only test passes with the guard
// deleted and proves nothing.
func TestLoadDir_DefaultBranchOnlySkipsInheritedBranches(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "global.yaml"), []byte(`branches:
  - main
poll_interval: 1m
`), 0644); err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(dir, "repo1")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config.yaml"), []byte(`repos:
  repo1:
    url: git@github.com:test/repo1.git
    ssh_key_path: `+keyFile+`
    local_path: `+dir+`/repo1-clone
    poll_interval: 1m
    default_branch_only: true
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	repo, ok := cfg.Repos["repo1"]
	if !ok {
		t.Fatal("repo1 missing after load")
	}
	if len(repo.Branches) != 0 {
		t.Fatalf("branches = %v, want none inherited", repo.Branches)
	}
	if !repo.IsDefaultBranchOnly() {
		t.Fatal("default_branch_only was lost")
	}

	// Validate drops repos it rejects, so surviving it is the real assertion.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate after load: %v", err)
	}
	if _, ok := cfg.Repos["repo1"]; !ok {
		t.Fatal("repo1 was dropped: inherited branches must not fail a default_branch_only repo")
	}
}

// Same for tags: a defaults block naming them must not fail a repo that never
// asked for them.
func TestLoadDir_DefaultBranchOnlySkipsInheritedTags(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "global.yaml"), []byte("tags:\n  - "+tagSemverGlob+"\npoll_interval: 1m\n"), 0644); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(dir, "repo1")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config.yaml"), []byte(`repos:
  repo1:
    url: git@github.com:test/repo1.git
    ssh_key_path: `+keyFile+`
    local_path: `+dir+`/repo1-clone
    poll_interval: 1m
    default_branch_only: true
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate after load: %v", err)
	}
	if _, ok := cfg.Repos["repo1"]; !ok {
		t.Fatal("repo1 was dropped: inherited tags must not fail a default_branch_only repo")
	}
}

// The guard must still reject branches the operator set on the repo itself,
// as opposed to ones inherited from defaults.
func TestLoadDir_DefaultBranchOnlyRejectsExplicitBranches(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(dir, "repo1")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config.yaml"), []byte(`repos:
  repo1:
    url: git@github.com:test/repo1.git
    ssh_key_path: `+keyFile+`
    local_path: `+dir+`/repo1-clone
    poll_interval: 1m
    default_branch_only: true
    branches:
      - main
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Load does not validate; Validate is what drops an invalid repo.
	_ = cfg.Validate()
	if _, ok := cfg.Repos["repo1"]; ok {
		t.Fatal("expected repo1 to be dropped for setting branches alongside default_branch_only")
	}
}

// --- OpenBao config tests ---

func validOpenBao() *OpenBaoConfig {
	return &OpenBaoConfig{
		Enabled:                 true,
		Addr:                    "http://openbao.openbao:8200",
		Role:                    "gfetch",
		ServiceAccountTokenPath: "/var/run/secrets/openbao/token",
	}
}

// openBaoTestConfig builds a Config with one SSH repo and the given OpenBao
// block. Use it to avoid repeating the ~10-line literal in every test.
func openBaoTestConfig(ob *OpenBaoConfig, sshKeyPath string) *Config {
	return &Config{
		OpenBao: ob,
		Repos: map[string]RepoConfig{testRepoName: {
			RepoDefaults: RepoDefaults{
				SSHKeyPath:   sshKeyPath,
				LocalPath:    testLocalPath,
				PollInterval: Duration(30 * time.Second),
				Branches:     []Pattern{{Raw: branchMain}},
			},
			Name: testRepoName,
			URL:  testRepoURL,
		}},
	}
}

func TestValidate_OpenBaoEnabled_ValidRepoGetsOpenBaoCopy(t *testing.T) {
	ob := validOpenBao()
	cfg := openBaoTestConfig(ob, "ssh-keys/acme/kubeaid")

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}

	repo := cfg.Repos[testRepoName]
	if repo.OpenBao == nil {
		t.Fatal("expected repo.OpenBao to be set")
	}
	if repo.OpenBao.Addr != ob.Addr {
		t.Errorf("addr = %q, want %q", repo.OpenBao.Addr, ob.Addr)
	}
	if repo.OpenBao.KVMount != "kv" {
		t.Errorf("kv_mount = %q, want %q (default)", repo.OpenBao.KVMount, "kv")
	}
	if repo.OpenBao.Field != "private_key" {
		t.Errorf("field = %q, want %q (default)", repo.OpenBao.Field, "private_key")
	}
}

func TestValidate_OpenBaoOff_FilePathUnchanged(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		openbao *OpenBaoConfig
	}{
		{"disabled", &OpenBaoConfig{Enabled: false}},
		{"absent", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := openBaoTestConfig(tt.openbao, keyFile)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("expected valid config, got: %v", err)
			}
			repo := cfg.Repos[testRepoName]
			if repo.OpenBao != nil {
				t.Errorf("expected repo.OpenBao to be nil when openbao is %s", tt.name)
			}
		})
	}
}

func TestValidate_OpenBaoInvalidBlock_HardError(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(ob *OpenBaoConfig)
	}{
		{"empty addr", func(ob *OpenBaoConfig) { ob.Addr = "" }},
		{"no scheme", func(ob *OpenBaoConfig) { ob.Addr = "openbao:8200" }},
		{"ftp scheme", func(ob *OpenBaoConfig) { ob.Addr = "ftp://openbao:8200" }},
		{"no host", func(ob *OpenBaoConfig) { ob.Addr = "http://" }},
		{"empty role", func(ob *OpenBaoConfig) { ob.Role = "" }},
		{"empty service_account_token_path", func(ob *OpenBaoConfig) { ob.ServiceAccountTokenPath = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ob := validOpenBao()
			tt.mutate(ob)
			cfg := openBaoTestConfig(ob, "ssh-keys/acme")
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected hard error")
			}
			if _, ok := errors.AsType[*PartialValidateError](err); ok {
				t.Fatal("expected hard error, got PartialValidateError")
			}
		})
	}
}

func TestValidate_OpenBaoBadKVPath_PerRepoFailure(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"leading slash", "/ssh-keys/acme"},
		{"double dot", "ssh-keys/../acme"},
		{"single dot segment", "a/./b"},
		{"just dot", "."},
		{"empty segment", "ssh-keys//acme"},
		{"bad charset", "ssh-keys/acme@host"},
		{"data prefix", "data/ssh-keys/acme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ob := validOpenBao()
			cfg := &Config{
				OpenBao: ob,
				Repos: map[string]RepoConfig{
					"bad": {
						RepoDefaults: RepoDefaults{
							SSHKeyPath:   tt.path,
							LocalPath:    testLocalPath,
							PollInterval: Duration(30 * time.Second),
							Branches:     []Pattern{{Raw: branchMain}},
						},
						Name: "bad",
						URL:  testRepoURL,
					},
					goodRepoName: {
						RepoDefaults: RepoDefaults{
							SSHKeyPath:   "ssh-keys/valid",
							LocalPath:    "/tmp/good",
							PollInterval: Duration(30 * time.Second),
							Branches:     []Pattern{{Raw: branchMain}},
						},
						Name: goodRepoName,
						URL:  testRepoURL,
					},
				},
			}

			before := testutil.ToFloat64(telemetry.ConfigRepoValidateFailuresTotal.WithLabelValues("bad"))

			err := cfg.Validate()
			var partial *PartialValidateError
			if !errors.As(err, &partial) {
				t.Fatalf("expected PartialValidateError, got %T: %v", err, err)
			}
			if _, ok := partial.Failures["bad"]; !ok {
				t.Error("expected failure for 'bad' repo")
			}
			if _, ok := cfg.Repos[goodRepoName]; !ok {
				t.Error("expected 'good' repo to survive")
			}

			after := testutil.ToFloat64(telemetry.ConfigRepoValidateFailuresTotal.WithLabelValues("bad"))
			if after != before+1 {
				t.Errorf("metric: got %v, want %v", after, before+1)
			}
		})
	}
}

func TestValidate_OpenBaoEnabled_SSHRepoMissingKeyPath_PerRepoFailure(t *testing.T) {
	cfg := openBaoTestConfig(validOpenBao(), "")
	// Empty ssh_key_path to trigger the "is required" error.
	r := cfg.Repos[testRepoName]
	r.SSHKeyPath = ""
	cfg.Repos[testRepoName] = r

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing ssh_key_path")
	}
}

func TestValidate_OpenBaoEnabled_HTTPSRepoUnaffected(t *testing.T) {
	ob := validOpenBao()
	cfg := &Config{
		OpenBao: ob,
		Repos: map[string]RepoConfig{"public": {
			RepoDefaults: RepoDefaults{
				LocalPath:    testLocalPath,
				PollInterval: Duration(30 * time.Second),
				Branches:     []Pattern{{Raw: branchMain}},
			},
			Name: "public",
			URL:  "https://github.com/git/git.git",
		}},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected HTTPS repo to pass with openbao enabled, got: %v", err)
	}
	repo := cfg.Repos["public"]
	if repo.OpenBao != nil {
		t.Error("HTTPS repo should not get OpenBao config")
	}
}

func TestLoad_SingleFile_OpenBaoFlowsToRepo(t *testing.T) {
	content := `openbao:
  enabled: true
  addr: http://openbao:8200
  role: gfetch
  service_account_token_path: /var/run/secrets/token
repos:
  test-repo:
    url: git@github.com:test/repo.git
    ssh_key_path: ssh-keys/test
    local_path: /tmp/test
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
	if cfg.OpenBao == nil {
		t.Fatal("expected OpenBao to be loaded")
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	repo := cfg.Repos["test-repo"]
	if repo.OpenBao == nil {
		t.Fatal("expected repo.OpenBao to be set after validate")
	}
	if repo.OpenBao.KVMount != "kv" {
		t.Errorf("kv_mount default = %q, want %q", repo.OpenBao.KVMount, "kv")
	}
	if repo.OpenBao.Field != "private_key" {
		t.Errorf("field default = %q, want %q", repo.OpenBao.Field, "private_key")
	}
}

func TestLoadDir_GlobalYAMLOpenBaoHonored(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "global.yaml"), []byte(`ssh_key_path: ssh-keys/default
local_path: /tmp/repos
poll_interval: 5m
branches:
  - "*"
openbao:
  enabled: true
  addr: http://openbao:8200
  role: gfetch
  service_account_token_path: /var/run/secrets/token
`), 0644); err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(dir, "repo1")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config.yaml"), []byte(`repos:
  repo1:
    url: git@github.com:test/repo1.git
    ssh_key_path: ssh-keys/repo1
    branches:
      - main
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenBao == nil {
		t.Fatal("expected OpenBao from global.yaml")
	}
	if !cfg.OpenBao.Enabled {
		t.Fatal("expected OpenBao.Enabled = true")
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	repo := cfg.Repos["repo1"]
	if repo.OpenBao == nil {
		t.Fatal("expected repo.OpenBao to be set")
	}
}

func TestLoadDir_PerRepoOpenBaoIgnored(t *testing.T) {
	dir := t.TempDir()

	repoDir := filepath.Join(dir, "repo1")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repoDir, "config.yaml")
	// Per-repo config has an openbao block, which should be ignored.
	if err := os.WriteFile(configPath, []byte(`openbao:
  enabled: true
  addr: http://openbao:8200
  role: gfetch
  service_account_token_path: /var/run/secrets/token
repos:
  repo1:
    url: git@github.com:test/repo1.git
    ssh_key_path: /tmp/key
    local_path: /tmp/repo1
    poll_interval: 1m
    branches:
      - main
`), 0644); err != nil {
		t.Fatal(err)
	}

	// Write the key file so file-path validation passes.
	if err := os.WriteFile("/tmp/key", []byte("fake"), 0600); err != nil {
		// Can't write to /tmp/key; use a temp dir instead.
		t.Skip("cannot write test key file")
	}
	t.Cleanup(func() { _ = os.Remove("/tmp/key") })

	// Capture slog output to verify the warning.
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(old) })

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The per-repo openbao is ignored; cfg.OpenBao remains nil because there's
	// no global.yaml.
	if cfg.OpenBao != nil {
		t.Fatal("per-repo openbao should be ignored")
	}

	logged := buf.String()
	if !strings.Contains(logged, configPath) {
		t.Errorf("expected warning to mention per-repo config path %q, got: %s", configPath, logged)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	repo := cfg.Repos["repo1"]
	if repo.OpenBao != nil {
		t.Error("repo should not have OpenBao when per-repo block is ignored")
	}
}

func TestValidate_OpenBaoEnabled_SSHKeyPathInheritedFromDefaults(t *testing.T) {
	cfg := openBaoTestConfig(validOpenBao(), "")
	cfg.Defaults = &RepoDefaults{SSHKeyPath: "ssh-keys/default"}

	// Clear the repo's ssh_key_path so it inherits from defaults.
	r := cfg.Repos[testRepoName]
	r.SSHKeyPath = ""
	cfg.Repos[testRepoName] = r

	// Apply defaults as loadFile would.
	for name, repo := range cfg.Repos {
		applyDefaults(&repo, cfg.Defaults)
		cfg.Repos[name] = repo
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	repo := cfg.Repos[testRepoName]
	if repo.SSHKeyPath != "ssh-keys/default" {
		t.Errorf("ssh_key_path = %q, want %q", repo.SSHKeyPath, "ssh-keys/default")
	}
	if repo.OpenBao == nil {
		t.Fatal("expected repo.OpenBao to be set")
	}
}

func TestMarshal_OpenBaoBlockPresent_RepoOpenBaoAbsent(t *testing.T) {
	cfg := openBaoTestConfig(validOpenBao(), "ssh-keys/test")

	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	s := string(out)
	if !strings.Contains(s, "openbao:") {
		t.Error("expected openbao block in marshaled output")
	}
	if !strings.Contains(s, "ssh_key_path: ssh-keys/test") {
		t.Error("expected ssh_key_path in marshaled output")
	}
	// The per-repo OpenBao (yaml:"-") should not appear as a nested openbao
	// block under the repo. We check by parsing the output and inspecting the
	// repos section: no repo should have an "openbao" key.
	var parsed map[string]any
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	repos, ok := parsed["repos"].(map[string]any)
	if !ok {
		t.Fatal("expected repos map in marshaled output")
	}
	for name, raw := range repos {
		repoMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, has := repoMap["openbao"]; has {
			t.Errorf("repo %q has an openbao key in marshaled output (yaml:\"-\" should suppress it)", name)
		}
	}
}
