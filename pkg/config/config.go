package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultPollInterval is used when a repo does not specify a poll interval.
	DefaultPollInterval = 2 * time.Minute

	hoursPerDay = 24
)

// Config is the top-level configuration.
type Config struct {
	Defaults *RepoDefaults `yaml:"defaults,omitempty"`
	Repos    []RepoConfig  `yaml:"repos"`
}

// Duration is a wrapper around time.Duration that supports extra units like 'd'.
type Duration time.Duration

// UnmarshalYAML unmarshals a Duration from a string.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := ParseDuration(value.Value)
	if err != nil {
		return err
	}
	*d = Duration(dur)
	return nil
}

// MarshalYAML marshals a Duration to a string.
func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// RepoDefaults holds default values that are applied to repos missing those fields.
type RepoDefaults struct {
	SSHKeyPath    string    `yaml:"ssh_key_path"`
	SSHKnownHosts string    `yaml:"ssh_known_hosts"`
	LocalPath     string    `yaml:"local_path"`
	PollInterval  Duration  `yaml:"poll_interval"`
	Branches      []Pattern `yaml:"branches"`
	Tags          []Pattern `yaml:"tags"`
	OpenVox       *bool     `yaml:"openvox"`
	PruneStale    *bool     `yaml:"prune_stale"`
	StaleAge      Duration  `yaml:"stale_age"`
}

// RepoConfig defines the sync configuration for a single repository.
type RepoConfig struct {
	Name          string    `yaml:"name"`
	URL           string    `yaml:"url"`
	SSHKeyPath    string    `yaml:"ssh_key_path"`
	SSHKnownHosts string    `yaml:"ssh_known_hosts"`
	LocalPath     string    `yaml:"local_path"`
	PollInterval  Duration  `yaml:"poll_interval"`
	Branches      []Pattern `yaml:"branches"`
	Tags          []Pattern `yaml:"tags"`
	Checkout      string    `yaml:"checkout"`
	OpenVox       bool      `yaml:"openvox"`
	PruneStale    bool      `yaml:"prune_stale"`
	StaleAge      Duration  `yaml:"stale_age"`
}

// Pattern represents a matching pattern, either literal or regex.
// Used for both branch and tag matching.
type Pattern struct {
	Raw      string
	compiled *regexp.Regexp
}

// UnmarshalYAML unmarshals a Pattern from a plain YAML string.
func (p *Pattern) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("expected a string, got %v", value.Kind)
	}
	p.Raw = value.Value
	return nil
}

// MarshalYAML marshals a Pattern to a plain YAML string.
func (p Pattern) MarshalYAML() (interface{}, error) {
	return p.Raw, nil
}

// IsRegex returns true if the pattern is a regex (delimited by /).
func (p *Pattern) IsRegex() bool {
	return strings.HasPrefix(p.Raw, "/") && strings.HasSuffix(p.Raw, "/") && len(p.Raw) > 2
}

// Matches returns true if the given name matches this pattern.
func (p *Pattern) Matches(name string) bool {
	if p.Raw == "*" {
		return true
	}
	if p.IsRegex() {
		if p.compiled != nil {
			return p.compiled.MatchString(name)
		}
		return false
	}
	return p.Raw == name
}

// Compile compiles the regex pattern if applicable.
func (p *Pattern) Compile() error {
	if !p.IsRegex() {
		return nil
	}
	pattern := p.Raw[1 : len(p.Raw)-1]
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid regex %q: %w", p.Raw, err)
	}
	p.compiled = re
	return nil
}

// matchesAny returns true if the given name matches any of the patterns.
func matchesAny(name string, patterns []Pattern) bool {
	for i := range patterns {
		if patterns[i].Matches(name) {
			return true
		}
	}
	return false
}

// IsHTTPS returns true if the repo URL uses HTTP or HTTPS.
func (r *RepoConfig) IsHTTPS() bool {
	return strings.HasPrefix(r.URL, "https://") || strings.HasPrefix(r.URL, "http://")
}

// ParseDuration parses a duration string, adding support for 'd' (days).
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	original := s
	var multiplier time.Duration
	switch {
	case strings.HasSuffix(s, "d"):
		multiplier = hoursPerDay * time.Hour
		s = s[:len(s)-1]
	default:
		return time.ParseDuration(s)
	}

	var val float64
	if _, err := fmt.Sscanf(s, "%f", &val); err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", original, err)
	}
	return time.Duration(val * float64(multiplier)), nil
}

// Load reads and parses configuration from a file or directory.
// If path is a file, it loads a single YAML config.
// If path is a directory, it loads global.yaml for defaults and */config.yaml for repos.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("accessing config path: %w", err)
	}
	if info.IsDir() {
		return loadDir(path)
	}
	return loadFile(path)
}

// loadFile reads and parses a single YAML config file.
func loadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	// First try to unmarshal into the standard Config struct which has "defaults" key.
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	// Also support top-level fields for backward compatibility.
	var raw struct {
		SSHKeyPath    string    `yaml:"ssh_key_path"`
		SSHKnownHosts string    `yaml:"ssh_known_hosts"`
		LocalPath     string    `yaml:"local_path"`
		PollInterval  Duration  `yaml:"poll_interval"`
		Branches      []Pattern `yaml:"branches"`
		Tags          []Pattern `yaml:"tags"`
		OpenVox       *bool     `yaml:"openvox"`
		PruneStale    *bool     `yaml:"prune_stale"`
		StaleAge      Duration  `yaml:"stale_age"`
	}
	if err := yaml.Unmarshal(data, &raw); err == nil {
		// If explicit defaults key is missing, but top-level fields are present, use them.
		if cfg.Defaults == nil {
			hasTopLevel := raw.SSHKeyPath != "" || raw.SSHKnownHosts != "" || raw.LocalPath != "" ||
				raw.PollInterval != 0 || len(raw.Branches) > 0 || len(raw.Tags) > 0 || raw.OpenVox != nil ||
				raw.PruneStale != nil || raw.StaleAge != 0

			if hasTopLevel {
				cfg.Defaults = &RepoDefaults{
					SSHKeyPath:    raw.SSHKeyPath,
					SSHKnownHosts: raw.SSHKnownHosts,
					LocalPath:     raw.LocalPath,
					PollInterval:  Duration(raw.PollInterval),
					Branches:      raw.Branches,
					Tags:          raw.Tags,
					OpenVox:       raw.OpenVox,
					PruneStale:    raw.PruneStale,
					StaleAge:      Duration(raw.StaleAge),
				}
			}
		}
	}

	// Apply defaults to each repo.
	if cfg.Defaults != nil {
		for i := range cfg.Repos {
			applyDefaults(&cfg.Repos[i], cfg.Defaults)
		}
	}

	return &cfg, nil
}

// loadDir loads configuration from a directory structure.
func loadDir(dir string) (*Config, error) {
	var defaults RepoDefaults

	// Load global.yaml if it exists.
	var hasDefaults bool
	globalPath := filepath.Join(dir, "global.yaml")
	if data, err := os.ReadFile(globalPath); err == nil {
		if err := yaml.Unmarshal(data, &defaults); err != nil {
			return nil, fmt.Errorf("parsing global.yaml: %w", err)
		}
		hasDefaults = true
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading global.yaml: %w", err)
	}

	// Scan */config.yaml for per-repo configs.
	matches, err := filepath.Glob(filepath.Join(dir, "*", "config.yaml"))
	if err != nil {
		return nil, fmt.Errorf("scanning config directory: %w", err)
	}

	cfg := &Config{}
	if hasDefaults {
		cfg.Defaults = &defaults
	}
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", match, err)
		}
		var sub Config
		if err := yaml.Unmarshal(data, &sub); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", match, err)
		}
		cfg.Repos = append(cfg.Repos, sub.Repos...)
	}

	// Apply global defaults to each repo.
	for i := range cfg.Repos {
		applyDefaults(&cfg.Repos[i], &defaults)
	}

	return cfg, nil
}

// applyDefaults fills empty repo fields from global defaults.
func applyDefaults(repo *RepoConfig, defaults *RepoDefaults) {
	if repo.SSHKeyPath == "" && defaults.SSHKeyPath != "" {
		repo.SSHKeyPath = defaults.SSHKeyPath
	}
	if repo.SSHKnownHosts == "" && defaults.SSHKnownHosts != "" {
		repo.SSHKnownHosts = defaults.SSHKnownHosts
	}
	if repo.PollInterval == 0 && defaults.PollInterval != 0 {
		repo.PollInterval = defaults.PollInterval
	}
	if repo.LocalPath == "" && defaults.LocalPath != "" {
		repo.LocalPath = defaults.LocalPath
	}
	if len(repo.Branches) == 0 && len(defaults.Branches) > 0 {
		repo.Branches = defaults.Branches
	}
	if len(repo.Tags) == 0 && len(defaults.Tags) > 0 {
		repo.Tags = defaults.Tags
	}
	if defaults.OpenVox != nil && !repo.OpenVox {
		repo.OpenVox = *defaults.OpenVox
	}
	if defaults.PruneStale != nil && !repo.PruneStale {
		repo.PruneStale = *defaults.PruneStale
	}
	if repo.StaleAge == 0 && defaults.StaleAge != 0 {
		repo.StaleAge = defaults.StaleAge
	}
}

// Validate checks the configuration for required fields and compiles regex patterns.
func (c *Config) Validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("no repos configured")
	}

	names := make(map[string]bool)
	for i := range c.Repos {
		r := &c.Repos[i]
		if err := c.validateRepo(r, i, names); err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateRepo(r *RepoConfig, index int, names map[string]bool) error {
	if r.Name == "" {
		return fmt.Errorf("repo at index %d: name is required", index)
	}
	if names[r.Name] {
		return fmt.Errorf("duplicate repo name: %s", r.Name)
	}
	names[r.Name] = true

	if r.URL == "" {
		return fmt.Errorf("repo %s: url is required", r.Name)
	}
	if err := c.validateAuth(r); err != nil {
		return err
	}

	if r.LocalPath == "" {
		return fmt.Errorf("repo %s: local_path is required", r.Name)
	}
	if r.PollInterval == 0 {
		r.PollInterval = Duration(DefaultPollInterval)
	} else if time.Duration(r.PollInterval) < 10*time.Second {
		return fmt.Errorf("repo %s: poll_interval must be at least 10s, got %s", r.Name, time.Duration(r.PollInterval))
	}

	if r.PruneStale && r.StaleAge == 0 {
		// Default to 180 days (approx 6 months)
		r.StaleAge = Duration(180 * 24 * time.Hour)
	}
	if len(r.Branches) == 0 && len(r.Tags) == 0 {
		return fmt.Errorf("repo %s: at least one branch or tag pattern is required", r.Name)
	}

	for j := range r.Branches {
		if err := r.Branches[j].Compile(); err != nil {
			return fmt.Errorf("repo %s: %w", r.Name, err)
		}
	}
	for j := range r.Tags {
		if err := r.Tags[j].Compile(); err != nil {
			return fmt.Errorf("repo %s: %w", r.Name, err)
		}
	}

	if r.OpenVox && r.Checkout != "" {
		slog.Warn("repo has both openvox and checkout set; checkout will be ignored in openvox mode", "repo", r.Name)
	}

	if r.Checkout != "" && !r.OpenVox {
		if !matchesAny(r.Checkout, r.Branches) && !matchesAny(r.Checkout, r.Tags) {
			return fmt.Errorf("repo %s: checkout %q does not match any configured branch or tag pattern", r.Name, r.Checkout)
		}
	}
	return nil
}

func (*Config) validateAuth(r *RepoConfig) error {
	if r.IsHTTPS() {
		return CheckHTTPSAccessible(r.Name, r.URL)
	}
	if r.SSHKeyPath == "" {
		return fmt.Errorf("repo %s: ssh_key_path is required", r.Name)
	}
	if _, err := os.Stat(r.SSHKeyPath); err != nil {
		return fmt.Errorf("repo %s: ssh key not found at %s: %w", r.Name, r.SSHKeyPath, err)
	}
	return nil
}
