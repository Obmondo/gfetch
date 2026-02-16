# gfetch

A CLI tool that selectively mirrors remote Git repositories to local paths based on YAML configuration.

## Features

- **Selective sync** — choose exactly which branches and tags to mirror using exact names or regex patterns
- **Pruning** — detect and remove local branches/tags that no longer match any configured pattern
- **Daemon mode** — run as a foreground polling service with per-repo poll intervals
- **SSH and HTTPS auth** — private repos via SSH key, public repos via anonymous HTTPS
- **Working tree checkout** — optionally keep a working tree checked out on a specific branch or tag
- **Lightweight clones** — repos are initialized empty and only configured refs are fetched

## Quick Start

```bash
# Install
go install github.com/ashish1099/gfetch/cmd/gfetch@latest

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

## Installation

### From source

```bash
git clone https://github.com/ashish1099/gfetch.git
cd gfetch
go build -o gfetch ./cmd/gfetch
```

### With `go install`

```bash
go install github.com/ashish1099/gfetch/cmd/gfetch@latest
```

### Releases

Pre-built binaries for Linux and macOS (amd64/arm64) are available via [GoReleaser](https://github.com/ashish1099/gfetch/releases). Each release includes a `checksums.txt` for verification.

## Configuration

gfetch reads a YAML config file (default: `config.yaml` in the current directory). Each entry in `repos` defines a repository to sync.

```yaml
repos:
  - name: my-service
    url: git@github.com:ashish1099/my-service.git
    ssh_key_path: /home/user/.ssh/id_ed25519
    local_path: /var/repos/my-service
    poll_interval: 5m
    checkout: main
    branches:
      - main
      - /^release-.*/        # regex pattern
    tags:
      - /^v[0-9]+\./
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
gfetch sync --prune --dry-run  # show what would be pruned without deleting
```

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | *(empty)* | Sync only the named repo |
| `--prune` | `false` | Delete local branches/tags that no longer match any pattern |
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
