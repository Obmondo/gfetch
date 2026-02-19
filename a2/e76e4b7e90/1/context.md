# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Enhance `SanitizeName` to Handle All Non-Alphanumeric Characters

## Context

`SanitizeName` in `pkg/gsync/openvox.go` converts Git ref names into Puppet environment directory names. Puppet environments only allow `[a-zA-Z0-9_]`. Currently the function only replaces `-` and `.` with `_`, but Git branch names can contain `/` (e.g., `feature/login`), `@`, `~`, `^`, `+`, spaces, and other characters. A branch like `feature/my-branch` would currently create a nested ...

### Prompt 2

commit this and its already in staged

