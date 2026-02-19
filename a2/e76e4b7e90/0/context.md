# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Fix: `gfetch cat` doesn't show global ssh_known_hosts

## Context
When using directory-mode config with a `global.yaml` that sets `ssh_known_hosts`, the `gfetch cat` command doesn't display the global value. This is because the `Config` struct (`pkg/config/config.go:20-22`) only has `Repos []RepoConfig` — the global defaults from `global.yaml` (or top-level single-file fields) are applied to repos via `applyDefaults` but then discarded. The marshaled output onl...

