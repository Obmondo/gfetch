# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Plan: Add `openvox` config key

## Context

gfetch currently syncs each repo into a single `local_path` directory with all branches/tags as git refs. For Puppet/OpenVox environment management, each branch/tag needs to be a **separate directory** with sanitized names (hyphens and dots replaced with underscores). This feature adds an `openvox` boolean to the YAML config that enables this behavior.

## Design

When `openvox: true`, instead of one git repo at `local_...

### Prompt 2

tag sync v1.4.4: checkout tag v1.4.4: reset v1.4.4: invalid reset option: object not found

### Prompt 3

[Request interrupted by user for tool use]

