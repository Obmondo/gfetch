# AGENTS.md

Project context and conventions for AI coding agents working on gfetch.

## Project Overview

gfetch is a Go CLI tool that selectively mirrors remote Git repositories to local paths based on YAML configuration. It supports one-shot sync, a polling daemon mode, SSH/HTTPS auth, and branch/tag filtering via exact names or regex patterns.

## Project Structure

```tree
cmd/gfetch/main.go          # Entry point — calls cli.NewRootCmd().Execute()
internal/cli/
  root.go                     # Root cobra command, persistent flags (--config, --log-level)
  sync.go                     # "sync" subcommand (--repo, --prune, --dry-run)
  daemon.go                   # "daemon" subcommand (foreground polling)
  validate.go                 # "validate-config" subcommand
  cat.go                      # "cat" subcommand — print resolved config as YAML
  version.go                  # "version" subcommand + Version/Commit/Date vars
pkg/config/
  config.go                   # Config/RepoDefaults/RepoConfig/OpenBaoConfig/Pattern structs; RepoConfig embeds RepoDefaults inline; OpenBaoConfig embeds secrets.OpenBaoConfig with Enabled flag; Load(), Validate(), validateRepo(), validateAuth(), validateOpenBaoConfig(), validateKVPath(); PartialValidateError for partial-validate tolerance; per-repo-subdir (<dir>/<repo>/config.yaml) directory discovery — strict fail-fast on load
  duration.go                 # ParseDuration() — support for s, m, h, d
  url.go                      # CheckHTTPSAccessible() helper
  config_test.go              # Config unit tests
pkg/secrets/
  store.go                    # Store — concurrent-safe secret reader: cache per openbao config and path handing out copies, with a generation per path (the first of two concurrent reads of a path to finish caches, a slower one never overwrites it); Refetch (replaces a cached key only when it changed), Invalidate (evicts a path under every config); one client per openbao config, built on first use and kept; sentinels ErrNotFound/ErrForbidden/ErrUnavailable/ErrRejected/ErrInvalidSecret/ErrClientConfig (a client that cannot be built from the config); a failed read, client build included, is counted on the secret fetch failure metric
  openbao.go                  # OpenBao KV v2 client — lazy kubernetes-auth login, background renewal from 2/3 lease (reads keep the current token; a failed renewal is retried at most once per policyDenialWindow; only a missing or expired token makes a read wait for a login: one login runs at a time on its own context without the mutex, every read needing it waits on it until its own ctx ends, and a failed one is answered from memory for loginFailureBackoff instead of retried per read), 403 backstop re-login through that same shared login rate-limited to reloginCooldown (a third of the lease when that is shorter, token age taken at the 403), error mapping to sentinels (an unreadable service-account token file is ErrUnavailable)
  store_test.go               # httptest-based tests for login, shared login and its failure backoff, read, cache-first, invalidation, background token renewal, concurrency, metrics
pkg/gsync/
  auth.go                     # resolveAuth() — SSH key from file, OpenBao secret, or nil (HTTPS); sshAuth (file), openBaoAuth = parseSSHKey + withKnownHosts (in-memory PEM, from Read or the bytes Refetch returned; the endpoint is parsed before the OpenBao read); preferredHostKeyAlgorithms + mergeAlgorithms (OpenSSH-style dynamic per-host promotion); sshHostPort()
  auth_test.go                # SSH auth and host key algorithm tests
  errors.go                   # Classify(): maps sync errors to stable FailureCodes; secret errors are typed at resolveAuth
  known_hosts.go              # buildKnownHostsAuth() — host key callback + per-host algorithms via gitssh.NewKnownHostsDb
  syncer.go                   # Syncer, SyncAll(), SyncRepo(), syncBranches(), syncTagsWrapper(), handleCheckout(), ensureCloned()
  refs.go                     # resolveBranches/resolveTags/default branch + staleness and branch discovery helpers
  prune.go                    # shared prune helpers (PruneItems, deleteBranch, pruneOpenVoxDirs)
  branch.go                   # syncBranch(), checkoutRef()
  tag.go                      # syncTags(), resolveAndFilterTags(), fetchTags(), handleObsoleteTags()
  openvox.go                  # OpenVox sync flow and per-ref directory sync/prune logic
  syncer_test.go              # Integration tests using in-process bare repos
pkg/telemetry/
  telemetry.go                # Prometheus metrics definitions and registration (sync metrics + config reload metrics + secret fetch metrics)
pkg/daemon/
  constants.go                # failure code table: HTTP status + message per code
  scheduler.go                # Scheduler — gocron-backed periodic syncing; atomic config pointer + Reload() (cancel all jobs / swap config / reschedule with delayed first-fire)
  scheduler_test.go           # Scheduler + Reload diff tests
  server.go                   # HTTP server: /health, /metrics, /sync, /sync/{repo}, POST /reload
  server_test.go              # HTTP handler tests
config.example.yaml           # Annotated example configuration
testdata/config.yaml          # Test fixture
.goreleaser-github.yaml       # Release config used by the GitHub release workflow
.goreleaser-gitea.yaml        # Release config used by the Gitea release workflow (release disabled, Harbor images only)
cog.toml                      # cocogitto release config: `cog bump` tags the merged master commit and pushes it to GitHub and Gitea
Dockerfile                    # Multi-stage build (Go builder + Alpine runtime)
.github/workflows/
  test.yml                    # CI: tests, lint, fmt on push/PR
  release.yml                 # On v* tag: GoReleaser binaries/release + GHCR image
.gitea/workflows/
  release.yml                 # On v* tag: Harbor image only — the mirror runs nothing else
renovate.json                 # Renovate config (gomod, dockerfile, github-actions)
```

## Key Architectural Decisions

- **No full clone**: repos are initialized empty via `git.PlainInit` + `CreateRemote`, then only configured refs are fetched with narrow refspecs. This avoids downloading unnecessary data.
- **Ref-level sync**: branches are updated by setting local refs directly to remote hashes — no merge/pull. The working tree is only touched when `checkout` is configured.
- **Pre-sync stale skip**: when both prune and stale pruning are enabled, stale branches are skipped before branch sync to avoid unnecessary fetch/sync work.
- **Daemon scheduling**: `gocron/v2` runs one job per repo with singleton-reschedule mode (overlapping fires are coalesced). Shutdown is via context cancellation on SIGINT/SIGTERM. The scheduler keeps the current config behind an `atomic.Pointer[*config.Config]` so reloads swap the live view in a single atomic write; in-flight syncs already hold their `RepoConfig` snapshot and finish against the old version.
- **Live config reload (Prometheus-style)**: reloads are triggered explicitly — either `SIGHUP` (handled by `Scheduler.runReloadOnSignal`) or `POST /reload`. There is no filesystem watcher and no combined reload-then-sync endpoint: like Prometheus, gfetch trusts the operator (or the SaaS API) to trigger reload once their write is complete, avoiding races with partial writes / editor swap files / atomic-rename half-states. Both triggers run the same pipeline (`server.go::loadValidateAndReload`): `config.Load` → `Validate` → `Scheduler.Reload`. `Scheduler.Reload` is intentionally simple: it cancels every running gocron job, atomically swaps `s.cfg`, and re-schedules every repo from `newCfg` with a delayed first-fire (one full `poll_interval` out, via `scheduleJob(..., startNow=false)`). The delay is deliberate stampede protection — a single reload can flip many repos at once, and firing them all immediately would hit upstream in one burst. In-flight syncs are not interrupted: cancelling a gocron job does not kill the goroutine it already spawned, and `RunGuardedSync`'s per-repo guard prevents the post-reload fire from racing with an in-flight sync of the same repo.
- **Strict load, partial validate**: `loadDir` is fail-fast (master behaviour) — any unreadable file, malformed YAML, or duplicate repo name aborts the entire load with an error. The reload pipeline (`server.go::loadValidateAndReload`) treats fatal load errors as HTTP 500 and the previous config keeps running. `Config.Validate` *does* support partial degradation: per-repo validation failures are dropped from `c.Repos`, counted via `telemetry.ConfigRepoValidateFailuresTotal`, and surfaced as `*config.PartialValidateError`. The reloader and `/reload` handler use `errors.As` to treat partial-validate errors as warnings and proceed; the CLI surfaces them as normal errors. The "no repos configured" hard error is preserved for explicitly-empty input.
- **Directory-mode layout**: `loadDir` discovers per-repo configs at `<dir>/<repo>/config.yaml` via `filepath.Glob`. `<dir>/global.yaml` at the root carries shared defaults.
- **Pruning in daemon mode**: daemon passes `SyncOptions{}` to `SyncRepo`, which then promotes `repo.Prune`, `repo.PruneStale`, and `repo.StaleAge` from the repo config into the options. Set `prune: true` or `prune_stale: true` in config to enable pruning per-repo in daemon mode. `--prune` and `--dry-run` CLI flags remain exclusive to the `sync` subcommand.
- **SSH HostKeyAlgorithms**: `gitssh.NewKnownHostsDb` produces both the host-key callback and the list of algorithms our known_hosts has entries for at the dialed `host:port`. `mergeAlgorithms` puts those first, then appends the OpenSSH 9.x default order (Ed25519/ECDSA/RSA-SHA2 plus their cert variants) — mimicking OpenSSH's dynamic per-host HostKeyAlgorithms promotion so negotiation lands on a key type we can verify. Plain `ssh-rsa` (SHA-1) and `ssh-dss` are intentionally omitted.
- **Cache-first SSH key fetch**: when `openbao.enabled: true`, the SSH private key is fetched from OpenBao KV v2 on first use and cached in memory per path. On reload only the key paths of repos added, removed or re-keyed are evicted (`InvalidateSecrets`), except a path an unchanged repo still reads; the OpenBao client and token survive, and a key rotated in place is picked up by the ssh-auth refetch. Each openbao config has its own client and cache entries, so after a reload that changes the block, syncs still running on the old one and syncs on the new one do not evict each other's client or cache. An SSH auth failure at the first network op (listRemoteRefs / loadResolverRepoAndRefs; `ensureCloned` is local only, so this holds on a fresh clone too) refetches the key, replaces the cached one only if it changed, and retries once with the key the refetch returned if it differs (self-heals a missed reload); the sync then carries on, object-store repair included, with the refetched key. A key that does not parse is evicted (it never reaches the network, so no auth failure would refetch it), so a value fixed in OpenBao is read on the next sync. There is no stale-key fallback: if OpenBao is down on a cache miss, the sync fails.
- **HTTP error contract**: `/sync`, `/sync/{repo}` and `/reload` failures return a status per failure class (503 secret backend unavailable / git host unreachable / shutting down, 422 git host rejection, missing, invalid, denied or rejected secret, or invalid config, 409 sync in progress, 404 unknown repo, 500 otherwise, including an OpenBao client that cannot be built from the config, `secret_config_invalid`). `error` keeps the raw error chain; `code` (one table in `pkg/daemon/constants.go`) and `message` are the stable surface callers should match on. An OpenBao request timeout or an unreadable service-account token file is `secret_unavailable`; a policy denial (`secret_forbidden`) or a request OpenBao rejects (`secret_rejected`) does not clear on a retry, so it is 422; a request the caller cancelled is `internal`; a git-host dial failure or timeout, or an SSH handshake the host closes early (EOF), is `git_unreachable`, 503 since it is transient; a repo URL that does not parse is `internal`, not `git_unreachable`, even though its url.Error satisfies net.Error.
- **Package Naming**: `pkg/gsync` is used for git sync logic to avoid conflict with the standard `sync` package. `pkg/telemetry` is used for metrics.
- **Concurrent OpenVox Sync**: When `openvox: true` is set, branches and tags are synced in parallel using a worker pool (default 5 workers). A mutex protects the shared resolver repo (`.gfetch-meta`) from concurrent access conflicts.
- **Index auto-repair**: `checkoutRefContext` automatically detects corrupted, truncated, or unknown `.git/index` files (including `unexpected EOF`, `EOF`, and `index.ErrMalformedSignature`) and deletes/rebuilds them on the fly.
- **Ref exclusion**: `exclude_branches` and `exclude_tags` allow operators to skip syncing specific branches or tags (e.g., during local experiments); excluded refs are automatically exempt from pruning.

## Build & Test

```bash
# Build
go build -o gfetch ./cmd/gfetch

# Keep code updated with current Go standards after edits
go fix ./...

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

# Format check (must be clean before committing)
gofmt -l .           # list files with issues
gofmt -w .           # fix in place

# Lint (must pass before committing — config: .golangci.yml)
golangci-lint run ./...
```

For AI coding agents: after making code changes, run `go fix ./...` before tests/lint.

## Code Conventions

- **Go version**: 1.27 (see `go.mod`)
- **Module path**: `github.com/obmondo/gfetch`
- **Logging**: `log/slog` with text handler to stderr. Per-repo code builds a `log *slog.Logger` with `slog.With("repo", …)` and passes it down as a parameter (e.g. `runStandardSync`, `refreshAuthAfterSSHFailure`, the openvox workers); a type that owns its logging holds a struct logger (e.g. `secrets.Store`); package-level code without a per-repo context (e.g. config loading, the scheduler) logs via the slog default. Use `.With("key", value)` for structured fields.
- **CLI framework**: `github.com/spf13/cobra`. Commands are defined in `internal/cli/` with `newXxxCmd()` factory functions. Flags use package-level vars.
- **Config parsing**: `gopkg.in/yaml.v3`. Config validation is manual in `Config.Validate()` — no struct validation tags.
- **Git operations**: `github.com/go-git/go-git/v5`. All git work is done through the go-git library, not by shelling out to git.
- **Error handling**: errors are returned up the call stack, wrapped with `fmt.Errorf("context: %w", err)`. The `sync` command exits with code 1 if any repo has an error.
- **Tests**: standard `testing` package, table-driven tests, `t.TempDir()` for temp dirs. Sync tests use in-process bare git repos created via `initBareAndClone()` helper. No external test dependencies or test frameworks.
- **No Makefile**: build and test with standard `go` commands.

## Release

- Cut with `cog bump --auto` (`cog.toml`) on a `master` that matches GitHub's; a pre-bump hook refuses otherwise. It tags the merged commit, with no bump commit or changelog since changes land through GitHub PRs, and pushes `master` and the tag to `origin` (GitHub) and `gitea` (the mirror, kept up to date only by this push).
- On GitHub the tag runs `.github/workflows/release.yml` (GoReleaser binaries, GitHub release, GHCR image); on Gitea it runs `.gitea/workflows/release.yml` (Harbor image only).
- `--auto` derives the version from the commit types since the last tag (`feat` minor, `fix` patch, `!` major), so commit subjects must follow the style below.

## Git commit style

- **Mandatory User Approval**: NEVER commit changes unless the user explicitly asks you to.
- **Signed Commits**: Always sign commits when requested (ensure GPG signing is enabled or use `-S`).
- **Pre-commit checks**: Before every commit, run `gofmt -w .` and `golangci-lint run ./...`. Both must be clean — no formatting diffs, no lint issues.
- Follow [Conventional Commits](https://www.conventionalcommits.org/):

```git
<type>(<scope>): <subject>

[optional body]
```

### Types

| Type | When to use |
| ------ | ------------- |
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

```git
chore!: rename module path to github.com/obmondo/gfetch
feat(api)!: remove deprecated endpoint
```

### Scope

Use the package or subsystem name — keep it short:

```git
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
- `exclude_branches` and `exclude_tags` (if set) are compiled and validated as patterns, and rejected alongside `default_branch_only`
- `prune` (bool) enables obsolete-ref pruning (branches/tags no longer matching any pattern); defaults to false
- `prune_stale` (bool) and `stale_age` (duration) enable inactivity-based pruning; `prune_stale` only has effect when `prune: true` is also set (a warning is logged otherwise)
- Regex patterns (wrapped in `/`) are compiled and validated
- SSH URLs require `ssh_key_path`. When `openbao.enabled: true`, it is shape-checked as a KV path (charset `[A-Za-z0-9._/-]`, no leading `/`, no `..` or empty segments, no `data/` prefix); otherwise it must point to an existing file
- HTTPS URLs must be publicly accessible (HEAD request returns 200)
- `checkout` (if set) must match at least one branch or tag pattern
- `openbao` block with `enabled: true` must have a valid `addr` (http/https URL with host), `role`, and `service_account_token_path`. An invalid block is a hard config error (not per-repo). Defaults: `kv_mount=kv`, `field=private_key`. The `openbao` block in a per-repo `*/config.yaml` is ignored; only honored in `global.yaml` or at the single-file top level.

## Pattern System

Patterns appear in `branches` and `tags` YAML lists:

- **Exact**: plain string, matched with `==`
- **Wildcard**: `*` matches everything
- **Regex**: wrapped in `/` delimiters (e.g., `/^release-.*/`), compiled with `regexp.Compile`, matched with `MatchString`
- `//` is treated as a literal, not an empty regex
- `Pattern.Compile()` must be called before `Matches()` for regex patterns — `Validate()` handles this

## Auth System

- **SSH**: any non-HTTPS URL. When `openbao.enabled: true`, the key is fetched from OpenBao KV v2 and parsed in memory via `parseSSHKey` + `withKnownHosts` (cache-first; see decision above); otherwise uses `go-git/go-git/v5/plumbing/transport/ssh.NewPublicKeysFromFile("git", keyPath, "")`. No passphrase support. SSH auth failure with an OpenBao key triggers one refetch-retry cycle (the retry uses the key the refetch returned and is skipped when it is unchanged); file-based keys are not retried.
- **Validation**: KV paths reject `.` and `..` segments; a per-repo `openbao` block in directory mode is ignored with a warning.
- **HTTPS**: URLs starting with `https://` or `http://`. Auth is `nil` (anonymous). Only public repos are supported.

## Known future work

- **go-git v6 needs a rewrite, not a bump.** `Syncer.New` sets
  `transport.UnsupportedCapabilities` to re-enable the multi_ack capabilities.
  Without them Azure DevOps replies to a fetch with an empty pack and no error:
  the refs get written, no objects arrive, and checkout then fails with "object
  not found" forever, since the refs already point at the right commits. v6
  reworks the transport layer and removes that global, so the assignment will
  not compile and the fetch, clone and capability plumbing all need reworking.
  Re-verify the Azure DevOps behaviour against whatever replaces it before
  upgrading. v6 was alpha as of Sep 2026.
