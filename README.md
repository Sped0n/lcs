# LCS

LCS caches selected Git repositories and Google toolchain downloads. It runs as an unprivileged Linux user service on `127.0.0.1:7777`.

## Build

Install Go 1.26 and Git, then build the binary:

```bash
go build -o lcsctl ./cmd/lcsctl
install -Dm0755 lcsctl "$HOME/.local/bin/lcsctl"
```

SQLite uses a pure-Go driver. It needs no database server or C library.

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

The only keys are `cache_size` and `cache`. Sizes use decimal units with `B`, `KB`, `MB`, `GB`, or `TB` suffixes. LCS rejects unknown keys and cache entries. An empty `cache` list keeps the proxy running without caching downloads.

The [example configuration](examples/config.toml) includes the larger CHIP submodules for ESP32 and Linux.

Each target documents what it caches and how to select it:

- [Git](internal/targets/git/README.md) caches repositories selected by `git:<url>` entries.
- [Google toolchain](internal/targets/chip-google-toolchain/README.md) caches recognized CIPD downloads selected by `chip-google-toolchain`.

## Run

Run the server in the foreground:

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

Change `ExecStart` in the copied file if the binary is not at `~/.local/bin/lcsctl`. Then start the service:

```bash
systemctl --user daemon-reload
systemctl --user enable --now lcs.service
```

Check the service and follow its logs:

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

With the daemon running, activate LCS in your Bash or Zsh shell:

```bash
eval "$(lcsctl shell)"
```

`shell` reads the running daemon's cache selections. It does not read a local configuration file. Only `serve` accepts `--config`.

For direnv, put the same command in the project's `.envrc`, then run `direnv allow`. Direnv restores the previous environment when you leave the directory.

Activation sets uppercase and lowercase HTTP and HTTPS proxy variables. It merges the existing `NO_PROXY` lists and excludes the LCS endpoint. It preserves active Git environment entries. Repeating the command does not add duplicate rewrites.

`lcsctl shell` writes executable statements to stdout and warnings to stderr. It does not start a subshell or change Git configuration files, shell profiles, or certificate stores. See the [Google toolchain trust setup](internal/targets/chip-google-toolchain/README.md#certificate-trust) for HTTPS interception.

## Check or clear the cache

```bash
lcsctl status
lcsctl clear
```

The `shell`, `status`, and `clear` commands use a mode-0600 Unix socket in the state directory. LCS has no TCP status or control endpoint. Clear requires a running service.

Status reports stored bytes by target, staging bytes, hits, misses, and traffic saved. Counters persist across restarts. Each target documents its counters. Requests outside the selected targets do not count as hits or misses.

Clear removes inactive entries immediately and active entries after their readers finish. Downloads already in progress cannot publish afterward. Clear keeps the configuration, CA, database, and counters.

## Storage

Default paths:

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

State and cache directories must be separate and private. Only one service can own either directory at a time. The server generates the CA once and keeps it across restarts. Only the owner can access the private key, which stays outside the cache directory.

All targets share the `cache_size` budget, including staging files and refresh copies. LCS reserves space before writes. LRU eviction starts near 90% usage and targets 80%. Entries in use stay in place. Downloads that cannot fit continue without caching.

The budget counts logical file bytes, not filesystem blocks or metadata. Use a filesystem quota if you need a hard disk cap. Target READMEs describe temporary storage growth that can exceed a reservation.

At startup, LCS removes abandoned staging files and unpublished entries. It reconciles missing files and stored sizes, then resets active references. Eviction never removes the CA or SQLite state.

## Network limits

HTTP requests and CONNECT tunnels can use custom TCP ports, including `6688`. Custom-port CONNECT requests pass through without TLS interception or caching. Cache selection does not restrict which public ports the proxy can reach.

The proxy checks DNS answers and connects to the checked addresses. It rejects loopback, private, link-local, and reserved addresses. These checks also apply to redirect targets the proxy can inspect.

LCS cannot inspect redirects inside TLS tunnels. It still checks subsequent proxied connections. LCS does not support remote deployment or isolate local users. Other users on the same machine can access the loopback listener.

## Verification

Install the pre-commit hook and run all checks through the Nix development shell:

```bash
nix develop -c prek install
nix develop -c prek run --all-files
```

The hooks format Go and Nix files, run tests with race detection, and run `go vet` and `golangci-lint`. GitHub Actions runs the same checks on pushes and pull requests. The development shell includes Git, Bash, and Zsh for integration tests.

Shell tests check that activation uses the daemon's selections and fails without a running daemon. Target READMEs describe protocol coverage and unverified download paths.

Local checks covered Linux amd64 and arm64 builds and the example unit's systemd syntax. The arm64 runtime, systemd deployment, and direnv activation with a running daemon remain unverified. No installation-time or bandwidth benchmark is available.
