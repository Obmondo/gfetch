# gfetch

[![GitHub Tests](https://github.com/obmondo/gfetch/actions/workflows/test.yml/badge.svg)](https://github.com/obmondo/gfetch/actions/workflows/test.yml)
[![GitHub Docker](https://github.com/obmondo/gfetch/actions/workflows/docker.yml/badge.svg)](https://github.com/obmondo/gfetch/actions/workflows/docker.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/obmondo/gfetch)](https://goreportcard.com/report/github.com/obmondo/gfetch)

A CLI tool that selectively mirrors remote Git repositories to local paths based on YAML configuration.

## Features

- **Selective sync** — choose exactly which branches and tags to mirror using exact names, wildcards (`*`), or regex patterns
- **Pruning** — detect and remove local branches/tags that no longer match any configured pattern
- **Stale pruning** — optionally remove inactive branches that have no new commits in a specified period (e.g., last 6 months)
- **Daemon mode** — run as a foreground polling service with per-repo poll intervals
- **SSH and HTTPS auth** — private repos via SSH key, public repos via anonymous HTTPS
- **Working tree checkout** — optionally keep a working tree checked out on a specific branch or tag
- **OpenVox mode** — create per-branch/tag directories with sanitized names, ideal for Puppet environments
- **Lightweight clones** — repos are initialized empty and only configured refs are fetched

## Quick Start

```bash
# Install
go install github.com/obmondo/gfetch/cmd/gfetch@latest

# Create a config file
cat <<'EOF' > config.yaml
repos:
  - name: my-repo
    url: https://github.com/example/repo.git
    local_path: /var/repos/my-repo
    poll_interval: 5m
    branches:
      - main
EOF

# Run a one-shot sync
gfetch sync
```

Or run with Docker:

```bash
docker run -v /path/to/config.yaml:/home/gfetch/config.yaml \
           -v /var/repos:/var/repos \
           ghcr.io/obmondo/gfetch daemon
```

## Installation

### From source

```bash
git clone https://github.com/obmondo/gfetch.git
cd gfetch
go build -o gfetch ./cmd/gfetch
```

### With `go install`

```bash
go install github.com/obmondo/gfetch/cmd/gfetch@latest
```

### Releases

Pre-built binaries for Linux and macOS (amd64/arm64) are available via [GitHub Releases](https://github.com/obmondo/gfetch/releases). Each release includes a `checksums.txt` for verification.

### Docker

A pre-built image is published to the GitHub Container Registry on every tagged release.

```bash
# Pull the latest image
docker pull ghcr.io/obmondo/gfetch

# Run the daemon with config and repo storage mounted
docker run -v /path/to/config.yaml:/home/gfetch/config.yaml \
           -v /var/repos:/var/repos \
           ghcr.io/obmondo/gfetch daemon
```

To build locally:

```bash
docker build -t gfetch .

# With version info
docker build --build-arg VERSION=1.0.0 \
             --build-arg COMMIT=$(git rev-parse --short HEAD) \
             --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
             -t gfetch .
```

## Configuration

gfetch reads a YAML config file (default: `config.yaml` in the current directory). Use a `defaults:` key to share settings across multiple repositories.

```yaml
defaults:
  ssh_key_path: /home/gfetch/.ssh/id_rsa
  poll_interval: 10m
  prune_stale: true             # remove branches with no commits in 6 months
  stale_age: 180d               # supports d (days)

# TODO: Add integration tests for real SSH Git repositories.

repos:
  - name: my-service
    url: git@github.com:obmondo/my-service.git
    branches:
      - main
      - /^release-.*/        # regex pattern
    tags:
      - "*"                  # wildcard matches all tags

  - name: internal-tool
    url: git@github.com:org/tool.git
    prune_stale: false          # override default for this repo
    branches:
      - main
```

See [docs/configuration.md](docs/configuration.md) for the full configuration reference, including all fields, pattern syntax, auth methods, and validation rules.

## Usage

### Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config`, `-c` | `config.yaml` | Path to config file |
| `--log-level` | `info` | Log level: `debug`, `info`, `warn`, `error` |

### `gfetch sync`

One-shot sync of all repos (or a specific repo).

```bash
gfetch sync                    # sync all repos
gfetch sync --repo my-service  # sync a specific repo
gfetch sync --prune            # sync and remove obsolete branches/tags
gfetch sync --prune-stale      # sync and remove branches with no commits in 6 months
gfetch sync --stale-age 30d    # custom threshold for stale pruning
gfetch sync --prune --dry-run  # show what would be pruned without deleting
```

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | *(empty)* | Sync only the named repo |
| `--prune` | `false` | Delete local branches/tags that no longer match any pattern |
| `--prune-stale` | `false` | Delete local branches with no commits in the last 6 months |
| `--stale-age` | `180d` | Custom age threshold for stale pruning (e.g., `30d`) |
| `--dry-run` | `false` | Show what would be pruned without actually deleting |

### `gfetch daemon`

Run as a foreground polling daemon. Each repo syncs immediately on start, then polls at its configured `poll_interval`. Shuts down gracefully on `SIGINT` or `SIGTERM`.

```bash
gfetch daemon
gfetch daemon --config /etc/gfetch/config.yaml --log-level debug
```

Pruning is not performed in daemon mode. The daemon does not reload config on changes — restart it to pick up new configuration.

### `gfetch validate-config`

Validate the config file and exit.

```bash
gfetch validate-config
gfetch validate-config -c /path/to/config.yaml
```

### `gfetch cat`

Print the fully resolved configuration as YAML. Loads the config, applies defaults, validates, and outputs the result to stdout.

```bash
gfetch cat
gfetch cat -c /path/to/config.yaml
```

### `gfetch version`

Print version information.

```bash
$ gfetch version
gfetch dev (commit: none, built: unknown)
```

Version, commit, and build date are injected at build time via ldflags when using GoReleaser or a manual build with `-ldflags`.

## Documentation

- [Configuration Reference](docs/configuration.md)

## License

TBD
