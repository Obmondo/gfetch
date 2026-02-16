package config

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPollInterval is used when a repo does not specify a poll interval.
const DefaultPollInterval = 2 * time.Minute

// Config is the top-level configuration.
type Config struct {
	Repos []RepoConfig `yaml:"repos"`
}

// RepoConfig defines the sync configuration for a single repository.
type RepoConfig struct {
	Name         string        `yaml:"name"`
	URL          string        `yaml:"url"`
	SSHKeyPath   string        `yaml:"ssh_key_path"`
	LocalPath    string        `yaml:"local_path"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Branches     []Pattern     `yaml:"branches"`
	Tags         []Pattern     `yaml:"tags"`
	Checkout     string        `yaml:"checkout"`
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

// IsRegex returns true if the pattern is a regex (delimited by /).
func (p *Pattern) IsRegex() bool {
	return strings.HasPrefix(p.Raw, "/") && strings.HasSuffix(p.Raw, "/") && len(p.Raw) > 2
}

// Matches returns true if the given name matches this pattern.
func (p *Pattern) Matches(name string) bool {
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

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	return &cfg, nil
}

// Validate checks the configuration for required fields and compiles regex patterns.
func (c *Config) Validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("no repos configured")
	}

	names := make(map[string]bool)
	for i := range c.Repos {
		r := &c.Repos[i]

		if r.Name == "" {
			return fmt.Errorf("repo at index %d: name is required", i)
		}
		if names[r.Name] {
			return fmt.Errorf("duplicate repo name: %s", r.Name)
		}
		names[r.Name] = true

		if r.URL == "" {
			return fmt.Errorf("repo %s: url is required", r.Name)
		}
		if r.IsHTTPS() {
			checkURL := r.URL
			checkURL = strings.TrimSuffix(checkURL, ".git")
			resp, err := http.Head(checkURL)
			if err != nil || resp.StatusCode != http.StatusOK {
				return fmt.Errorf("repo %s: HTTPS URL is not publicly accessible; only public repos are supported with HTTPS — use an SSH URL with ssh_key_path for private repos", r.Name)
			}
			resp.Body.Close()
		} else {
			if r.SSHKeyPath == "" {
				return fmt.Errorf("repo %s: ssh_key_path is required", r.Name)
			}
			if _, err := os.Stat(r.SSHKeyPath); err != nil {
				return fmt.Errorf("repo %s: ssh key not found at %s: %w", r.Name, r.SSHKeyPath, err)
			}
		}
		if r.LocalPath == "" {
			return fmt.Errorf("repo %s: local_path is required", r.Name)
		}
		if r.PollInterval == 0 {
			r.PollInterval = DefaultPollInterval
		} else if r.PollInterval < 10*time.Second {
			return fmt.Errorf("repo %s: poll_interval must be at least 10s, got %s", r.Name, r.PollInterval)
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

		if r.Checkout != "" {
			if !matchesAny(r.Checkout, r.Branches) && !matchesAny(r.Checkout, r.Tags) {
				return fmt.Errorf("repo %s: checkout %q does not match any configured branch or tag pattern", r.Name, r.Checkout)
			}
		}
	}

	return nil
}
