# Configuration Reference

gfetch is configured with a YAML file or a directory of YAML files. By default it looks for `config.yaml` in the current directory. Use `--config` / `-c` to specify a different path (file or directory).

## Configuration structure

gfetch uses a `defaults` key for shared settings and a `repos` key for individual repositories. The `repos` field MUST be a map, where each key is the repository name.

### Single-file mode

```yaml
defaults:
  ssh_key_path: /home/gfetch/.ssh/id_rsa
  poll_interval: 10m
  local_path: /var/repos

repos:
  my-repo:
    url: git@github.com:org/my-repo.git
    branches:
      - main
  another-repo:
    url: git@github.com:org/another.git
    branches:
      - develop
```

### Map format (Helm friendly)

Using a map for `repos` allows Helm to merge multiple `values.yaml` files correctly. Lists in Helm are always overwritten, but maps are merged by key.

### Directory mode

When `--config` points to a directory, gfetch loads `global.yaml` for shared defaults and discovers per-repo configs at `<dir>/<repo>/config.yaml`. A duplicate repo name across files is a fatal error.

```text
config.d/
├── global.yaml                 # shared defaults
├── repo-a/
│   └── config.yaml
├── repo-b/
│   └── config.yaml
└── repo-c/
    └── config.yaml
```

### Partial-validate tolerance

Loading the config is strict — any unreadable file, malformed YAML, or duplicate repo name aborts the load with a fatal error, and the previous config keeps running.

Validation, however, tolerates per-repo failures: a repo that fails `Validate` (missing required fields, bad regex, unreachable HTTPS URL, …) is dropped from the in-memory config, logged at WARN, counted in `gfetch_config_repo_validate_failures_total{repo}`, and surfaced as `*config.PartialValidateError`. The daemon's `POST /reload` and `SIGHUP` triggers treat partial-validate errors as warnings and continue with the surviving repos. The `validate-config` and `cat` CLI commands surface them as normal errors and exit non-zero. If all repos are dropped, the reload is aborted and the previous config keeps running.

## Fields

| Field | Type | Required | Description |
| ------- | ------ | ---------- | ------------- |
| (key) | string | Yes | The repository name. Max 64 characters. Allowed characters: `a-z`, `A-Z`, `0-9`, `.`, `_`, `-`. |
| `url` | string | Yes | Remote repository URL. Prefix `https://` for HTTPS; anything else for SSH. |
| `ssh_key_path` | string | Only for SSH | Filesystem path to a private SSH key file. When `openbao.enabled: true`, this is the KV secret path instead (e.g. `ssh-keys/acme/kubeaid`). Inheritable from `defaults`/`global.yaml`. |
| `ssh_known_hosts` | string | No | Extra SSH host key entries. Merged with built-in keys for GitHub, GitLab, Bitbucket, and Azure DevOps. |
| `local_path` | string | Yes | Local directory where the repo will be cloned and synced. |
| `poll_interval` | duration | Yes | How often the daemon polls this repo. Supports `30s`, `5m`, `1h`, `30d`. Minimum: `10s`. |
| `branches` | list of patterns | At least one of `branches` or `tags`, unless `default_branch_only` is set | Branch names or patterns to sync from the remote. |
| `exclude_branches` | list of patterns | No | Branch names or patterns to exclude from syncing. Excluded branches are also exempt from pruning. |
| `tags` | list of patterns | At least one of `branches` or `tags`, unless `default_branch_only` is set | Tag names or patterns to sync from the remote. |
| `exclude_tags` | list of patterns | No | Tag names or patterns to exclude from syncing. Excluded tags are also exempt from pruning. |
| `default_branch_only` | bool | No | Sync only the branch the remote's `HEAD` points at, whatever it is called. Cannot be combined with `branches`, `tags`, `checkout` or `openvox`. Default `false`. |
| `checkout` | string | No | A literal branch or tag name to check out in the working tree. |
| `openvox` | bool | No | Enable OpenVox mode. Each matching branch/tag gets its own subdirectory. |
| `openvox_max_workers` | int | No | OpenVox-only worker concurrency per repo. Default `5`, valid range `1..64`. |
| `production_alias` | bool | No | OpenVox-only. If true and upstream has no `production` branch, create/update a `production` symlink to the upstream default branch directory. |
| `prune` | bool | No | Remove local branches/tags no longer matching any configured pattern. Required for `prune_stale` to take effect. Default `false`. |
| `prune_stale` | bool | No | If true, local branches matching patterns but with no commits in `stale_age` will be pruned during sync (requires `prune: true`). When both are enabled, stale branches are also skipped before branch sync. Default `false`. |
| `stale_age` | duration | No | The period of inactivity (based on committer date) after which a branch is considered stale. Default `180d`. |

## Default Branch Only

Set `default_branch_only: true` to mirror just the branch the remote's `HEAD`
points at, without naming it:

```yaml
repos:
  customer-repo:
    url: git@github.com:org/repo.git
    local_path: /var/repos/customer-repo
    poll_interval: 5m
    default_branch_only: true
```

This is for callers that do not know the branch name up front — it may be
`main`, `master`, `trunk` or anything else — and want that one branch rather
than widening `branches` to `"*"` and mirroring everything.

The branch is resolved per sync from the remote's advertised `HEAD`, so if
upstream changes its default branch, the next sync follows it. The working tree
is checked out on that branch, which is the standard behaviour when no
`checkout` is configured.

It syncs the default branch and **nothing else**, so it cannot be combined with
the fields that name refs:

| Combined with | Result |
| --------------- | -------- |
| `branches` | Config error — the branch comes from the remote `HEAD`. |
| `tags` | Config error — the option means the default branch and nothing else. |
| `checkout` | Config error — the default branch is already checked out. |
| `openvox` | Config error — OpenVox syncs every matched branch into its own directory. |

A repo with `default_branch_only: true` also does not inherit `branches` or
`tags` from `defaults:`. Without that, a shared default would trip the rules
above and fail a config the operator never wrote that way.

Pruning interacts with it as follows:

- `prune: true` removes local branches other than the default branch, as usual.
  The branch just synced is never treated as obsolete.
- `prune_stale` has no effect in this mode. The only configured branch is the
  one just synced, and pruning it for being old would delete the sole branch
  the repo exists to mirror.

## Stale Pruning

Stale pruning allows gfetch to remove inactive branches from the local mirror, even if they still match a configured pattern (like `*`). This is especially useful for preventing local storage from growing indefinitely when using wildcards or broad regex patterns.

- **Check**: gfetch inspects the **committer date** of the tip of each local branch.
- **Threshold**: If the latest commit is older than `stale_age` (relative to the current time), the branch is identified as stale.
- **Action**: When `--prune-stale` is used (or `prune_stale: true` is set in the config), these branches are deleted.
- **Gate**: `prune_stale` only takes effect when `prune` is also enabled (via `--prune` flag or `prune: true` in config). If `prune_stale` is enabled without `prune`, a warning is logged and stale pruning is skipped.
- **Pre-sync optimization**: When both prune and prune-stale are enabled, stale branches are skipped before branch sync instead of being synced first and removed later. For the `sync` CLI this means passing both `--prune` and `--prune-stale`; in daemon/config mode, set both `prune: true` and `prune_stale: true`.
- **Safety**: The branch currently specified in the `checkout` field is **never** pruned, even if it is stale.

## Duration units

gfetch supports standard Go duration strings as well as human-friendly units for long-term configuration:

- `s` — seconds (e.g., `30s`)
- `m` — minutes (e.g., `5m`)
- `h` — hours (e.g., `24h`)
- `d` — days (e.g., `30d`)

## OpenVox Mode

When `openvox: true` is set on a repo, gfetch creates a separate subdirectory under `local_path` for each matching branch and tag. Each directory contains a fully checked-out working tree of that ref.

Directory names are sanitized: hyphens (`-`) and dots (`.`) are replaced with underscores (`_`). For example, a branch named `release-1.0` becomes the directory `release_1_0`.

If two refs produce the same sanitized name (e.g. `release-1` and `release.1` both become `release_1`), gfetch detects the collision and reports an error.

A hidden `.gfetch-meta` directory is created under `local_path` to store a resolver repo used for listing remote refs.

When `openvox` is enabled, the `checkout` field is ignored (a warning is logged if both are set).

`openvox_max_workers` controls concurrent per-ref workers in OpenVox mode. Higher values reduce wall-clock sync time for repos with many refs, but increase network and disk pressure.

If `production_alias: true` is set, gfetch ensures a `production` symlink points to the upstream default branch directory after branch sync. This is skipped when upstream already has a `production` branch.

Pruning (`prune: true` in config, or `--prune` via the sync CLI) in OpenVox mode removes directories that no longer correspond to any matched ref.

Stale pruning (`prune_stale: true`, requires `prune: true`) in OpenVox mode removes per-ref directories whose checked-out commit is older than `stale_age`.
When both prune and prune-stale are enabled, stale branches are also skipped before per-branch sync in OpenVox mode.

```yaml
repos:
  puppet-control:
    url: git@github.com:org/puppet-control.git
    ssh_key_path: /home/user/.ssh/id_ed25519
    local_path: /etc/puppetlabs/code/environments
    poll_interval: 2m
    openvox: true
    branches:
      - main
      - /^feature-.*/
    tags:
      - /^v[0-9]+\./
```

This produces a layout like:

```tree
/etc/puppetlabs/code/environments/
├── .gfetch-meta/          # internal resolver repo
├── main/                  # checked out from branch main
├── feature_foo/           # checked out from branch feature-foo
├── v1_0_0/                # checked out from tag v1.0.0
```

## Built-in SSH Host Keys

gfetch includes built-in SSH host keys for major Git hosting providers:

- **GitHub** (`github.com`, `ssh.github.com:443`)
- **GitLab** (`gitlab.com`)
- **Bitbucket** (`bitbucket.org`)
- **Azure DevOps** (`ssh.dev.azure.com`, `vs-ssh.visualstudio.com`)

These are always used for host key verification. Additional entries can be provided via the `ssh_known_hosts` field (per-repo or global).

## Pattern Syntax

Patterns are used in `branches` and `tags` lists to select which refs to sync.

### Exact match

A plain string matches a branch or tag name exactly.

```yaml
branches:
  - main
  - develop
tags:
  - v1.0.0
```

### Regex match

A string wrapped in `/` delimiters is treated as a Go regular expression.

```yaml
branches:
  - /^release-.*/       # matches release-1.0, release-2.3, etc.
tags:
  - /^v[0-9]+\./        # matches v1.0, v2.3.1, etc.
```

The delimiters are stripped before compilation. The regex is matched against the full ref name using Go's `regexp.MatchString`, so use anchors (`^`, `$`) when you need exact boundaries.

A pattern of `//` (empty regex) is treated as a literal string, not a regex.

## OpenBao Secret Backend

gfetch can fetch SSH private keys from an [OpenBao](https://openbao.org/) KV v2 secret backend instead of reading them from the filesystem. This is controlled by a global `openbao` block.

### Configuration

```yaml
openbao:
  enabled: true
  addr: http://openbao.openbao:8200
  role: gfetch
  service_account_token_path: /var/run/secrets/openbao/token
  kv_mount: kv            # default: kv
  field: private_key      # default: private_key
```

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `enabled` | Yes | `false` | Enable OpenBao for SSH key retrieval. |
| `addr` | Yes (when enabled) | — | OpenBao server URL (`http://` or `https://`). |
| `role` | Yes (when enabled) | — | Kubernetes auth role for login. |
| `service_account_token_path` | Yes (when enabled) | — | Path to the Kubernetes service account JWT file. |
| `kv_mount` | No | `kv` | KV v2 mount path. |
| `field` | No | `private_key` | Field name within the KV secret that holds the PEM-encoded private key. |

The `openbao` block is allowed at the top level of a single-file config and in `global.yaml` in directory mode. An `openbao` block in a per-repo `*/config.yaml` is ignored.

When `enabled: true`, per-repo `ssh_key_path` values are interpreted as KV secret paths instead of filesystem paths. The key is cached in memory per path after first fetch; the whole cache is cleared on every successful config reload (SIGHUP or POST /reload) -- the managing API reloads after deleting/regenerating since the path is the same. An SSH auth failure with an OpenBao key evicts the cached key, refetches it, and retries the network operation once (self-heals a missed reload). There is no stale-key fallback: if OpenBao is down on a cache miss, the sync fails. The key is never written to disk.

Token management: proactive re-login at 2/3 of the token lease duration, with a 403 backstop re-login rate-limited to one per minute. Recommend setting a short `token_ttl` on the Kubernetes auth role.

When `openbao.enabled` is set, `ssh_key_path` must be a KV path; an inherited `defaults.ssh_key_path` file path makes SSH repos fail validation (the path is validated as a KV path and dropped per repo).

When `enabled: false` or absent, `ssh_key_path` is a filesystem path as before, with no behavior change.

### Validation

An invalid `openbao` block (missing role, bad addr, etc.) is a **hard** config-level error. The reload is rejected and the previous config keeps running.

When enabled, per-repo `ssh_key_path` is shape-checked as a KV path:

- Must not be empty.
- Allowed characters: `A-Z`, `a-z`, `0-9`, `.`, `_`, `-`, `/`.
- Must not start with `/`.
- Must not contain empty segments (`//`), `.` segments, or `..` segments.
- Must not start with `data/` (the KV v2 API prefix is added automatically).

These shape failures are per-repo (the failing repo is dropped via `PartialValidateError`, other repos survive).

## Auth Methods

### SSH (private and public repos)

Use an SSH-style URL (e.g. `git@github.com:user/repo.git`) and provide `ssh_key_path`. When the `openbao` block is enabled, `ssh_key_path` is the KV secret path; otherwise it is the filesystem path to a private key file. The username is always `git`. Passphrase-protected keys are not supported.

The SSH key is fetched from OpenBao on first use and cached in memory per path. The cache is cleared on every successful config reload. An SSH auth failure with an OpenBao key evicts the cached key, refetches it, and retries once. The key is never written to disk.

Host key verification is always enabled using built-in keys. To add extra host keys (e.g. for a self-hosted Git server), use `ssh_known_hosts`:

```yaml
repos:
  private-repo:
    url: git@github.com:obmondo/my-service.git
    ssh_key_path: /home/user/.ssh/id_ed25519
    ssh_known_hosts: |              # optional: extra host keys
      custom-git.example.com ssh-ed25519 AAAA...
    local_path: /var/repos/my-service
    poll_interval: 5m
    branches:
      - main
```

### HTTPS (public repos only)

Use an `https://` or `http://` URL. No `ssh_key_path` is needed — authentication is anonymous. Only publicly accessible repositories are supported over HTTPS. During validation, gfetch makes an HTTP HEAD request to confirm the repo is reachable.

```yaml
repos:
  public-repo:
    url: https://github.com/git/git.git
    local_path: /var/repos/git
    poll_interval: 30m
    branches:
      - main
```

## Validation Rules

The config is validated when loaded. The following rules are enforced:

- `repos` must be a map and contain at least one entry.
- Repository names (map keys) must be ≤ 64 characters and contain only alphanumeric characters, dots, underscores, or hyphens.
- Each repo must have `url`, `local_path`, and `poll_interval` set.
- `poll_interval` must be at least `10s`.
- At least one of `branches` or `tags` must be non-empty, unless `default_branch_only: true` is set.
- `default_branch_only: true` is rejected alongside `branches`, `tags`, `checkout` or `openvox`. Each of those names the refs to sync, so they contradict the option rather than refine it — the config fails instead of silently ignoring what you wrote.
- All regex patterns must be valid Go regular expressions.
- If `url` is an SSH URL, `ssh_key_path` must be set. When `openbao.enabled: true`, it is shape-checked as a KV path; otherwise the file must exist on disk.
- If `url` is an HTTPS URL, the repo must be publicly accessible (HTTP 200 on HEAD request).
- If `checkout` is set, it must match at least one configured branch or tag pattern (not enforced when `openvox` is enabled).
- If both `openvox` and `checkout` are set, a warning is logged and `checkout` is ignored.
- `prune_stale: true` requires `prune: true` to take effect. If `prune_stale` is set without `prune`, a warning is logged and stale pruning is skipped.
- An `openbao` block with `enabled: true` must have a valid `addr` (http/https URL with host), `role`, and `service_account_token_path`. An invalid block is a hard config error.

Run `gfetch validate-config` to check your config file without performing any sync.

## CLI Commands

### `gfetch cat`

Prints the fully resolved configuration as YAML. In directory mode, global defaults are merged before printing.

```bash
gfetch cat -c config.yaml       # single-file mode
gfetch cat -c config.d/         # directory mode
```

## Daemon HTTP API

The daemon serves HTTP on its `--listen-addr` (default `:8080`):

| Method | Path | Description |
| -------- | ------ | ------------- |
| `GET` | `/health` | Returns `200` with `{"status":"ok"}` when running. Suitable as a Kubernetes readiness/liveness probe. |
| `GET` | `/metrics` | Prometheus metrics endpoint. |
| `POST` | `/reload` | Re-read the config from disk, validate it, and replace every running job. Response body is `{"repos": [...]}` listing the repos now managed. |
| `POST` | `/sync` | Trigger a manual sync of every configured repo. |
| `POST` | `/sync/{repo}` | Trigger a manual sync of a single repo. |

If you need to pick up a config change before syncing, call `POST /reload` first (or send `SIGHUP`), then `POST /sync`.

## Live config reload

When run as `gfetch daemon`, gfetch follows Prometheus's reload model: the config is **not** watched on the filesystem. Reloads are triggered explicitly by one of:

- `SIGHUP` to the gfetch process — convenient for traditional / CLI deployments and for config-management tooling (Ansible, systemd reload, etc.).
- `POST /reload` on the HTTP server — convenient for cross-pod / SaaS-API deployments where signaling is awkward. The endpoint is always enabled.

This avoids races with partial writes, editor swap files, and atomic-rename half-states; the writer always knows when its write is complete and can trigger reload deterministically.

Each reload performs `Load` → `Validate` → replace-all:

- Every running gocron job is cancelled.
- The live config pointer is atomically swapped.
- Every repo in the new config is re-scheduled with a delayed first-fire (one full `poll_interval` out — not immediate). This is deliberate stampede protection: a reload can flip many repos at once and firing them all immediately would hit upstream in one burst.

In-flight syncs are NOT interrupted. Cancelling a gocron job does not kill the goroutine it already spawned; in-flight tasks finish against the `RepoConfig` snapshot they were called with. A per-repo guard prevents the post-reload fire from racing with an in-flight sync of the same repo.

Reload failures (load, validate, apply) are logged and counted; the previous config keeps running. Per-repo validation failures are tolerated (see "Partial-validate tolerance" above) — only a completely empty result after partial-validate aborts the reload.

To pick up a config change and sync immediately, call `POST /reload` then `POST /sync` (or call `POST /sync` and wait for the next scheduled fire — Reload itself schedules every repo one full `poll_interval` out, no immediate fire).

## Prometheus Metrics

In addition to the per-sync metrics, the daemon exposes the following config-reload metrics:

| Metric | Type | Labels | Description |
| -------- | ------ | -------- | ------------- |
| `gfetch_config_reloads_total` | counter | — | Successful config reloads. |
| `gfetch_config_reload_failures_total` | counter | `reason` (`load`, `validate`, `apply`) | Config reload failures by stage. |
| `gfetch_config_repo_validate_failures_total` | counter | `repo` | Per-repo validation failures (the repo is dropped from the running config). |
| `gfetch_config_last_reload_timestamp` | gauge | — | Unix timestamp of the last successful reload. |
| `gfetch_config_managed_repos` | gauge | — | Number of repos currently managed by the daemon. |
| `gfetch_secret_fetch_failures_total` | counter | `repo`, `reason` (`not_found`, `forbidden`, `unavailable`, `invalid`, `rejected`) | Secret fetch failures per repo and reason. |
| `gfetch_secret_auth_refetch_total` | counter | `repo` | Times a cached secret key was evicted and refetched after an SSH authentication failure. |

## Complete Example

```yaml
defaults:
  ssh_key_path: /home/ashish/.ssh/id_ed25519
  poll_interval: 5m
  local_path: /var/repos
  prune: true                    # remove refs no longer matching any pattern
  prune_stale: true              # also remove inactive branches (requires prune: true)
  stale_age: 180d

repos:
  # Private repo via SSH (requires ssh_key_path)
  my-service:
    url: git@github.com:obmondo/my-service.git
    checkout: main                # working tree stays on this branch/tag
    branches:
      - main
      - develop
      - /^release-.*/              # regex (delimited by /)
    tags:
      - v1.0.0                     # exact match
      - /^v[0-9]+\./               # regex (delimited by /)

  # Public repo via HTTPS (no ssh_key_path needed)
  git-mirror:
    url: https://github.com/git/git.git
    local_path: /var/repos/git
    poll_interval: 30m
    branches:
      - main
    tags:
      - /^v[0-9]+\./
```
