# LCS

LCS caches selected Git repositories and Google toolchain downloads. It runs as an unprivileged Linux user service on `127.0.0.1:7777`.

## Build

Install Go 1.26 and Git, then build the binary:

```bash
go build -o lcsctl ./cmd/lcsctl
install -Dm0755 lcsctl "$HOME/.local/bin/lcsctl"
```

SQLite runs inside the binary. The build needs no database server or C compiler.

LCS targets Linux amd64 and arm64. To cross-compile for arm64:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/lcsctl
```

## Configure

Create `$XDG_CONFIG_HOME/lcs/config.toml`. If `XDG_CONFIG_HOME` is unset, use `~/.config/lcs/config.toml`.

```toml
cache_size = "50GB"

cache = [
  "git:https://github.com/project-chip/connectedhomeip.git",
  "chip-google-toolchain",
]
```

The only configuration keys are `cache_size` and `cache`. Sizes use decimal units, with `B`, `KB`, `MB`, `GB`, or `TB` suffixes. LCS rejects unknown keys and unsupported entries. An empty `cache` list leaves the proxy running without caching downloads.

Each target documents its configuration, behavior, and limits:

- [Git](internal/targets/git/README.md) caches repositories selected by `git:<url>` entries.
- [Google toolchain](internal/targets/chip-google-toolchain/README.md) caches recognized CIPD downloads selected by `chip-google-toolchain`.

## Run

To run the server in the foreground:

```bash
lcsctl serve
```

To use a different configuration file:

```bash
lcsctl serve --config ./config.toml
```

SIGINT and SIGTERM stop the server. Logs go to stderr.

For a systemd user service, copy the example unit:

```bash
install -Dm0644 examples/lcs.service "$HOME/.config/systemd/user/lcs.service"
```

Change `ExecStart` in the copied file if you installed the binary somewhere other than `~/.local/bin/lcsctl`. Then start the service:

```bash
systemctl --user daemon-reload
systemctl --user enable --now lcs.service
```

To check the service and follow its logs:

```bash
systemctl --user status lcs.service
journalctl --user -u lcs.service -f
```

To start user services without an active login session, an administrator can run:

```bash
sudo loginctl enable-linger "$USER"
```

LCS does not install the unit or change systemd settings.

## Activate

Run this command in your Bash or Zsh shell:

```bash
eval "$(lcsctl shell)"
```

The daemon must be running. `shell` uses its active cache selections, not a local configuration file. Only `serve` accepts `--config`.

For direnv, put the same command in the project's `.envrc`, then run `direnv allow`. Direnv restores the previous environment when you leave the directory.

Activation sets the uppercase and lowercase HTTP and HTTPS proxy variables. It merges the existing `NO_PROXY` lists and excludes the LCS endpoint. It preserves active Git environment entries and does not add duplicate rewrites when you repeat the command.

`lcsctl shell` writes executable shell statements to stdout and warnings to stderr. It does not start a subshell or change Git configuration files, shell profiles, or certificate stores. The [Google toolchain target](internal/targets/chip-google-toolchain/README.md#certificate-trust) describes the trust check for HTTPS interception.

## Check or clear the cache

```bash
lcsctl status
lcsctl clear
```

The `shell`, `status`, and `clear` commands contact a mode-0600 Unix socket in the state directory. LCS has no TCP status or control endpoint. The service must be running to clear the cache.

Status reports stored bytes by target, staging bytes, cache hits and misses, and traffic saved. Counters persist across restarts. Each target documents what its counters measure. Requests outside the selected targets do not count as hits or misses.

Clear removes inactive entries. It removes active entries after their readers finish and prevents downloads already in progress from publishing afterward. It keeps the configuration, CA, database, and counters.

## Storage

LCS uses these default paths:

```text
~/.config/lcs/config.toml
~/.local/state/lcs/ca.crt
~/.local/state/lcs/ca.key
~/.local/state/lcs/metadata.db
~/.local/state/lcs/control.sock
~/.cache/lcs/git/
~/.cache/lcs/artifacts/
~/.cache/lcs/tmp/
```

`XDG_CONFIG_HOME`, `XDG_STATE_HOME`, and `XDG_CACHE_HOME` override the default base directories. `STATE_DIRECTORY` and `CACHE_DIRECTORY` take precedence over the corresponding XDG variables. Each systemd variable must name one absolute directory.

State and cache directories must be separate and private. Only one service can own either directory at a time. The server generates the CA on its first startup and keeps it across restarts. The private key permits owner access only and stays outside the cache directory.

All targets share the `cache_size` budget, including staging files and refresh copies. LCS reserves space before writes. LRU eviction starts near 90% usage and targets 80%. LCS does not evict entries that readers still use. Downloads that cannot fit continue without caching.

The budget counts logical file bytes, not filesystem blocks or metadata. It is not a hard physical disk cap. Use a filesystem quota if you need one. Target documentation describes temporary storage growth that can exceed a reservation.

At startup, LCS removes abandoned staging files and unpublished entries. It reconciles missing files and stored sizes, then resets active references. Eviction never removes the CA or SQLite state.

## Network limits

The proxy permits public HTTP and HTTPS ports only. It checks DNS answers before connecting and uses those checked addresses. It rejects loopback, private, link-local, and reserved addresses, including redirect targets in requests it can inspect.

LCS cannot inspect redirects inside TLS tunnels. It still checks the addresses of subsequent proxied connections. Remote deployment and multi-user isolation are outside scope. Loopback access does not authenticate other users on the same machine.

## Verification

Install the pre-commit hook and run all checks through the Nix development shell:

```bash
nix develop -c prek install
nix develop -c prek run --all-files
```

The hooks format Go and Nix files, run tests with race detection, and run `go vet` and `golangci-lint`. GitHub Actions runs the same checks on pushes and pull requests. The development shell includes Git, Bash, and Zsh for the integration tests.

Tests use TLS connections, SQLite, Bash, and Zsh. They check that shell activation uses the daemon's selections and fails without a running daemon. Target READMEs describe their protocol tests and unverified download paths.

Linux amd64 and arm64 builds and the example unit's systemd syntax have been checked. The arm64 runtime, deployment through a systemd user service, and direnv activation with a running daemon remain unverified. No installation-time or bandwidth benchmark is available.
