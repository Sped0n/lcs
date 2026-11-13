# LCS

LCS caches selected public HTTPS Git repositories. It runs as a Linux user service at `127.0.0.1:7777` without root privileges.

## Build

Install Go 1.26 and Git, then build and install the binary:

```bash
go build -o lcsctl ./cmd/lcsctl
install -Dm0755 lcsctl "$HOME/.local/bin/lcsctl"
```

LCS supports Linux amd64 and arm64. To cross-compile for arm64:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/lcsctl
```

## Configure

Create `$XDG_CONFIG_HOME/lcs/config.toml`. If `XDG_CONFIG_HOME` is unset, use `~/.config/lcs/config.toml`.

```toml
cache_size = "50GB"

cache = [
    "git:https://github.com/project-chip/connectedhomeip.git",
    "git:https://github.com/espressif/connectedhomeip.git",
]
```

Set `cache_size` with decimal units using `B`, `KB`, `MB`, `GB`, or `TB`. Add each repository to `cache` as `git:<url>`. Use the same URL spelling as your Git commands, including the presence or absence of `.git`.

Each submodule needs its own entry. The [example configuration](examples/config.toml) includes larger CHIP submodules for ESP32 and Linux. Copy the entries you need into your configuration. An empty `cache` list disables caching.

Restart the service after changing the configuration. Activate your shell again if you change the selected repositories.

## Run

Install and start the systemd user service. If the binary is not at `~/.local/bin/lcsctl`, adjust `ExecStart` in the example unit first.

```bash
install -Dm0644 examples/lcs.service "$HOME/.config/systemd/user/lcs.service"
systemctl --user daemon-reload
systemctl --user enable --now lcs.service
```

View logs with `journalctl --user -u lcs.service -f`.

For a foreground server, run `lcsctl serve`. Add `--config ./config.toml` to use another configuration file. Press Ctrl+C to stop it.

To start without an active login session, an administrator can run `sudo loginctl enable-linger "$USER"`.

## Activate

With the daemon running, activate LCS in your Bash or Zsh shell:

```bash
eval "$(lcsctl shell)"
```

Then use your existing Git commands:

```bash
git clone https://github.com/project-chip/connectedhomeip.git
cd connectedhomeip
git submodule update --init --recursive
```

Activation affects the current shell. It does not edit Git configuration files or shell profiles. Unselected repositories and other programs keep their network settings. No CA installation is needed.

For direnv, add `eval "$(lcsctl shell)"` to the project's `.envrc`, then run `direnv allow`. Direnv restores the previous environment when you leave the directory.

Only `serve` accepts `--config`. The other commands use the running daemon.

## Check or clear the cache

```bash
lcsctl status
lcsctl clear
```

Status reports the service state and cache usage, including temporary downloads. It also shows hit and miss counters, which persist across restarts. Clear requires a running daemon.

Clear removes cached data without interrupting active readers. It deletes entries in use after their readers finish. It keeps the configuration and counters.

## Storage

Default locations:

```text
~/.config/lcs/config.toml
~/.local/state/lcs/
~/.cache/lcs/
```

Use `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, and `XDG_CACHE_HOME` to change the base directories. `STATE_DIRECTORY` and `CACHE_DIRECTORY` take precedence over the corresponding XDG variables. Each must name one absolute directory.

Keep the state and cache directories separate, with access restricted to your user. Do not nest one inside the other or share them between daemons.

The `cache_size` budget includes temporary downloads. Downloads that cannot fit continue without caching. Use a filesystem quota if you need a hard disk cap.

## Limits

LCS caches public HTTPS Git remotes on port 443. It does not cache private repositories, SSH remotes, or remotes on other ports.

Keep LCS on loopback. Other users on the same machine can access its Git listener. Do not expose it to a network.

See the [Git cache documentation](internal/git/README.md) for shallow fetch support, cache behavior, and counter definitions.
