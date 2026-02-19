# AGENTS.md

Project context and conventions for AI coding agents working on gfetch.

## Project Overview

gfetch is a Go CLI tool that selectively mirrors remote Git repositories to local paths based on YAML configuration. It supports one-shot sync, a polling daemon mode, SSH/HTTPS auth, and branch/tag filtering via exact names or regex patterns.

## Project Structure

```
cmd/gfetch/main.go          # Entry point — calls cli.NewRootCmd().Execute()
internal/cli/
  root.go                     # Root cobra command, persistent flags (--config, --log-level)
  sync.go                     # "sync" subcommand (--repo, --prune, --dry-run)
  daemon.go                   # "daemon" subcommand (foreground polling)
  validate.go                 # "validate-config" subcommand
  cat.go                      # "cat" subcommand — print resolved config as YAML
  version.go                  # "version" subcommand + Version/Commit/Date vars
pkg/config/
  config.go                   # Config/RepoConfig/Pattern structs, Load(), Validate(), validateRepo(), validateAuth()
  duration.go                 # ParseDuration() — support for s, m, h, d
  url.go                      # CheckHTTPSAccessible() helper
  config_test.go              # Config unit tests
pkg/gsync/
  auth.go                     # resolveAuth() — SSH key or nil (HTTPS)
  known_hosts.go              # SSH known_hosts callback helper
  syncer.go                   # Syncer, SyncAll(), SyncRepo(), syncBranches(), syncTagsWrapper(), handleCheckout(), ensureCloned()
  refs.go                     # resolveBranches/resolveTags/default branch + staleness and branch discovery helpers
  prune.go                    # shared prune helpers (PruneItems, deleteBranch, pruneOpenVoxDirs)
  branch.go                   # syncBranch(), checkoutRef()
  tag.go                      # syncTags(), resolveAndFilterTags(), fetchTags(), handleObsoleteTags()
  openvox.go                  # OpenVox sync flow and per-ref directory sync/prune logic
  syncer_test.go              # Integration tests using in-process bare repos
pkg/telemetry/
  telemetry.go                # Prometheus metrics definitions and registration
pkg/daemon/
  scheduler.go                # Scheduler — one goroutine per repo, signal handling
  scheduler_test.go           # Scheduler construction test
  server.go                   # HTTP server for health, metrics, and manual sync triggers
config.example.yaml           # Annotated example configuration
testdata/config.yaml          # Test fixture
.goreleaser.yaml              # Release config (linux/darwin, amd64/arm64)
Dockerfile                    # Multi-stage build (Go builder + Alpine runtime)
.gitea/workflows/
  docker.yaml                 # CI: Docker build on push/PR, push on tag
  test.yml                    # CI: Gitea tests on push/PR
renovate.json                 # Renovate config (gomod, dockerfile, github-actions)
```

## Key Architectural Decisions

- **No full clone**: repos are initialized empty via `git.PlainInit` + `CreateRemote`, then only configured refs are fetched with narrow refspecs. This avoids downloading unnecessary data.
- **Ref-level sync**: branches are updated by setting local refs directly to remote hashes — no merge/pull. The working tree is only touched when `checkout` is configured.
- **Pre-sync stale skip**: when both prune and stale pruning are enabled, stale branches are skipped before branch sync to avoid unnecessary fetch/sync work.
- **One goroutine per repo in daemon**: each repo polls independently with its own `time.Ticker`. Shutdown is via context cancellation on SIGINT/SIGTERM.
- **Pruning disabled in daemon mode**: daemon always uses `SyncOptions{}` (prune=false, pruneStale=false). Pruning is only available in the `sync` subcommand.
- **Package Naming**: `pkg/gsync` is used for git sync logic to avoid conflict with the standard `sync` package. `pkg/telemetry` is used for metrics.

## Build & Test

```bash
# Build
go build -o gfetch ./cmd/gfetch

# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run a specific package's tests
go test ./pkg/config/...
go test ./pkg/gsync/...

# TODO: Add integration tests using a real SSH Git repository (e.g., via a test container or mock server) to verify full SSH auth flow.

# Build with version info (like GoReleaser does)
go build -ldflags "-X github.com/obmondo/gfetch/internal/cli.Version=dev -X github.com/obmondo/gfetch/internal/cli.Commit=$(git rev-parse --short HEAD) -X github.com/obmondo/gfetch/internal/cli.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o gfetch ./cmd/gfetch
```

## Code Conventions

- **Go version**: 1.25 (see `go.mod`)
- **Module path**: `github.com/obmondo/gfetch`
- **Logging**: `log/slog` with text handler to stderr. Logger is passed through structs (e.g., `Syncer.logger`), not globals. Use `.With("key", value)` for structured fields.
- **CLI framework**: `github.com/spf13/cobra`. Commands are defined in `internal/cli/` with `newXxxCmd()` factory functions. Flags use package-level vars.
- **Config parsing**: `gopkg.in/yaml.v3`. Config validation is manual in `Config.Validate()` — no struct validation tags.
- **Git operations**: `github.com/go-git/go-git/v5`. All git work is done through the go-git library, not by shelling out to git.
- **Error handling**: errors are returned up the call stack, wrapped with `fmt.Errorf("context: %w", err)`. The `sync` command exits with code 1 if any repo has an error.
- **Tests**: standard `testing` package, table-driven tests, `t.TempDir()` for temp dirs. Sync tests use in-process bare git repos created via `initBareAndClone()` helper. No external test dependencies or test frameworks.
- **No Makefile**: build and test with standard `go` commands.

## Git commit style

- **Mandatory User Approval**: NEVER commit changes unless the user explicitly asks you to.
- **Signed Commits**: Always sign commits when requested (ensure GPG signing is enabled or use `-S`).
- Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <subject>

[optional body]
```

### Types

| Type | When to use |
|------|-------------|
| `feat` | New user-facing feature |
| `fix` | Bug fix |
| `docs` | Documentation only (README, configuration.md, code comments) |
| `chore` | Maintenance that is not a feature or fix (deps, config, rename, gitignore) |
| `ci` | CI/CD workflow changes (.github/workflows/) |
| `refactor` | Code restructuring with no behaviour change |
| `test` | Adding or fixing tests only |
| `perf` | Performance improvement |
| `build` | Build system changes (Makefile, Dockerfile, .goreleaser.yaml) |
| `style` | Formatting / whitespace only, no logic change |

### Breaking changes

Append `!` after the type/scope for breaking changes:

```
chore!: rename module path to github.com/obmondo/gfetch
feat(api)!: remove deprecated endpoint
```

### Scope

Use the package or subsystem name — keep it short:

```
fix(config): ...
feat(sync): ...
docs(readme): ...
ci(docker): ...
```

Omit scope when the change is repo-wide.

### Subject line rules

- Imperative mood, lowercase, no trailing period
- ≤ 72 characters
- Say *what* changed and *why*, not *how*

### Commit grouping

Commits should be logically atomic — one concern per commit. When a session touches multiple concerns, stage and commit them separately:

1. Code/logic fixes first (`fix`, `feat`, `refactor`)
2. Documentation second (`docs`)
3. Housekeeping last (`chore`, `ci`, `build`)

## Config Validation Rules (for reference when modifying config logic)

- `repos` must be non-empty
- Each repo requires: `name`, `url`, `local_path`, `poll_interval`
- `name` must be unique across repos
- `poll_interval` minimum is `10s`
- At least one of `branches` or `tags` must be non-empty
- `prune_stale` (bool) and `stale_age` (duration) enable inactivity-based pruning
- Regex patterns (wrapped in `/`) are compiled and validated
- SSH URLs require `ssh_key_path` pointing to an existing file
- HTTPS URLs must be publicly accessible (HEAD request returns 200)
- `checkout` (if set) must match at least one branch or tag pattern

## Pattern System

Patterns appear in `branches` and `tags` YAML lists:
- **Exact**: plain string, matched with `==`
- **Wildcard**: `*` matches everything
- **Regex**: wrapped in `/` delimiters (e.g., `/^release-.*/`), compiled with `regexp.Compile`, matched with `MatchString`
- `//` is treated as a literal, not an empty regex
- `Pattern.Compile()` must be called before `Matches()` for regex patterns — `Validate()` handles this

## Auth System

- **SSH**: any non-HTTPS URL. Uses `go-git/go-git/v5/plumbing/transport/ssh.NewPublicKeysFromFile("git", keyPath, "")`. No passphrase support.
- **HTTPS**: URLs starting with `https://` or `http://`. Auth is `nil` (anonymous). Only public repos are supported.
