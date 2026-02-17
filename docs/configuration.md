# Configuration Reference

gfetch is configured with a YAML file or a directory of YAML files. By default it looks for `config.yaml` in the current directory. Use `--config` / `-c` to specify a different path (file or directory).

## Single-file mode

```yaml
repos:
  - name: <string>
    url: <string>
    ssh_key_path: <string>       # required for SSH URLs
    ssh_known_hosts: <string>    # optional, extra SSH host key entries
    local_path: <string>
    poll_interval: <duration>
    checkout: <string>           # optional
    branches:
      - <pattern>
    tags:
      - <pattern>
```

The top-level key is `repos`, a list of repository configurations.

You can also set a top-level `ssh_known_hosts` key that applies to all repos in the file (per-repo values override it):

```yaml
ssh_known_hosts: |
  custom-git.example.com ssh-ed25519 AAAA...

repos:
  - name: my-repo
    ...
```

## Directory mode

Instead of a single file, `--config` can point to a directory. This is useful when managing many repos — each gets its own file.

### Layout

```
config.d/
├── global.yaml            # shared defaults (optional)
├── repo-a/
│   └── config.yaml        # repos: [{ name: repo-a, url: ..., branches: [...] }]
└── repo-b/
    └── config.yaml
```

### global.yaml

The `global.yaml` file in the config directory provides default values inherited by all repos. Any field set in a per-repo config overrides the global default.

```yaml
ssh_key_path: /home/user/.ssh/id_ed25519
ssh_known_hosts: |
  custom-git.example.com ssh-ed25519 AAAA...
poll_interval: 5m
local_path: /var/repos
```

Supported global fields: `ssh_key_path`, `ssh_known_hosts`, `poll_interval`, `local_path`.

### Per-repo config

Each subdirectory contains a `config.yaml` with the standard `repos:` structure:

```yaml
repos:
  - name: repo-a
    url: git@github.com:org/repo-a.git
    branches:
      - main
```

Fields left empty are filled from `global.yaml`.

## Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Unique identifier for the repo. Used with `--repo` flag to target a specific repo. |
| `url` | string | Yes | Remote repository URL. Prefix `https://` or `http://` for HTTPS mode; anything else (e.g. `git@github.com:...`) for SSH mode. |
| `ssh_key_path` | string | Only for SSH | Absolute path to a private SSH key file. Must exist on disk. Not used for HTTPS URLs. |
| `ssh_known_hosts` | string | No | Extra SSH host key entries (same format as `~/.ssh/known_hosts`). Merged with built-in keys for GitHub, GitLab, Bitbucket, and Azure DevOps. |
| `local_path` | string | Yes | Local directory where the repo will be cloned and synced. Created automatically if it doesn't exist. |
| `poll_interval` | duration | Yes | How often the daemon polls this repo. Go duration format (e.g. `30s`, `5m`, `1h`). Minimum: `10s`. |
| `branches` | list of patterns | At least one of `branches` or `tags` | Branch names or patterns to sync from the remote. |
| `tags` | list of patterns | At least one of `branches` or `tags` | Tag names or patterns to sync from the remote. |
| `checkout` | string | No | A literal branch or tag name to check out in the working tree. Must match at least one configured branch or tag pattern. |

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
- name: private-repo
  url: git@github.com:ashish1099/my-service.git
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
- name: public-repo
  url: https://github.com/git/git.git
  local_path: /var/repos/git
  poll_interval: 30m
  branches:
    - main
```

## Validation Rules

The config is validated when loaded. The following rules are enforced:

- `repos` must contain at least one entry.
- Each repo must have `name`, `url`, `local_path`, and `poll_interval` set.
- `name` must be unique across all repos.
- `poll_interval` must be at least `10s`.
- At least one of `branches` or `tags` must be non-empty.
- All regex patterns must be valid Go regular expressions.
- If `url` is an SSH URL, `ssh_key_path` must be set and the file must exist.
- If `url` is an HTTPS URL, the repo must be publicly accessible (HTTP 200 on HEAD request).
- If `checkout` is set, it must match at least one configured branch or tag pattern.

Run `gfetch validate-config` to check your config file without performing any sync.

## CLI Commands

### `gfetch cat`

Prints the fully resolved configuration as YAML. In directory mode, global defaults are applied to each repo before printing. Useful for verifying that inheritance works as expected.

```bash
gfetch cat -c config.yaml       # single-file mode
gfetch cat -c config.d/         # directory mode
```

## Complete Example

```yaml
repos:
  # Private repo via SSH (requires ssh_key_path)
  - name: my-service
    url: git@github.com:ashish1099/my-service.git
    ssh_key_path: /home/ashish/.ssh/id_ed25519
    local_path: /var/repos/my-service
    poll_interval: 5m
    checkout: main                # working tree stays on this branch/tag
    branches:
      - main
      - develop
      - /^release-.*/              # regex (delimited by /)
    tags:
      - v1.0.0                     # exact match
      - /^v[0-9]+\./               # regex (delimited by /)

  # Public repo via HTTPS (no ssh_key_path needed)
  - name: git
    url: https://github.com/git/git.git
    local_path: /var/repos/git
    poll_interval: 30m
    branches:
      - main
    tags:
      - /^v[0-9]+\./
```
