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

## Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| (key) | string | Yes | The repository name. Max 64 characters. Allowed characters: `a-z`, `A-Z`, `0-9`, `.`, `_`, `-`. |
| `url` | string | Yes | Remote repository URL. Prefix `https://` for HTTPS; anything else for SSH. |
| `ssh_key_path` | string | Only for SSH | Absolute path to a private SSH key file. |
| `ssh_known_hosts` | string | No | Extra SSH host key entries. Merged with built-in keys for GitHub, GitLab, Bitbucket, and Azure DevOps. |
| `local_path` | string | Yes | Local directory where the repo will be cloned and synced. |
| `poll_interval` | duration | Yes | How often the daemon polls this repo. Supports `30s`, `5m`, `1h`, `30d`. Minimum: `10s`. |
| `branches` | list of patterns | At least one of `branches` or `tags` | Branch names or patterns to sync from the remote. |
| `tags` | list of patterns | At least one of `branches` or `tags` | Tag names or patterns to sync from the remote. |
| `checkout` | string | No | A literal branch or tag name to check out in the working tree. |
| `openvox` | bool | No | Enable OpenVox mode. Each matching branch/tag gets its own subdirectory. |
| `prune` | bool | No | Remove local branches/tags no longer matching any configured pattern. Required for `prune_stale` to take effect. Default `false`. |
| `prune_stale` | bool | No | If true, local branches matching patterns but with no commits in `stale_age` will be pruned during sync (requires `prune: true`). When both are enabled, stale branches are also skipped before branch sync. Default `false`. |
| `stale_age` | duration | No | The period of inactivity (based on committer date) after which a branch is considered stale. Default `180d`. |

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

```
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

## Auth Methods

### SSH (private and public repos)

Use an SSH-style URL (e.g. `git@github.com:user/repo.git`) and provide `ssh_key_path` pointing to a private key file. The username is always `git`. Passphrase-protected keys are not supported.

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
- At least one of `branches` or `tags` must be non-empty.
- All regex patterns must be valid Go regular expressions.
- If `url` is an SSH URL, `ssh_key_path` must be set and the file must exist.
- If `url` is an HTTPS URL, the repo must be publicly accessible (HTTP 200 on HEAD request).
- If `checkout` is set, it must match at least one configured branch or tag pattern (not enforced when `openvox` is enabled).
- If both `openvox` and `checkout` are set, a warning is logged and `checkout` is ignored.
- `prune_stale: true` requires `prune: true` to take effect. If `prune_stale` is set without `prune`, a warning is logged and stale pruning is skipped.

Run `gfetch validate-config` to check your config file without performing any sync.

## CLI Commands

### `gfetch cat`

Prints the fully resolved configuration as YAML. In directory mode, global defaults are merged from subdirectories before printing.

```bash
gfetch cat -c config.yaml       # single-file mode
gfetch cat -c config.d/         # directory mode
```

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
