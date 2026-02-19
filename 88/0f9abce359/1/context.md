# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Issue #1: Stale pruning for OpenVox + remove top-level backward compat

## Context

**Issue**: [#1](https://github.com/Obmondo/gfetch/issues/1) — In OpenVox mode, `checkout` is ignored (each branch gets its own directory). The stale-prune safety check in normal mode relies on `repo.Checkout` to protect a branch from pruning, which doesn't work for OpenVox. Additionally, `syncRepoOpenVox` currently ignores `opts.PruneStale` entirely — no stale pruning happens ...

### Prompt 2

[Request interrupted by user for tool use]

### Prompt 3

the current code has multiple func which does the same job, like the git fetch is written in openvox and without openvox mode ?

