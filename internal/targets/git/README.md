# Git target

The Git target caches public HTTPS repositories selected by `git:<url>` entries. It uses bare mirrors and the system `git http-backend` command. It does not cache SSH remotes.

## Select repositories

Add each repository to `cache` in the [LCS configuration](../../../README.md#configure):

```toml
cache_size = "50GB"

cache = [
    "git:https://github.com/project-chip/connectedhomeip.git",
    "git:https://pigweed.googlesource.com/pigweed/pigweed.git",
]
```

Use the same URL spelling as your Git commands, including the presence or absence of `.git`. Cache entries must use HTTPS on port 443 and have no credentials, query, or fragment. HTTPS remotes on other ports pass through without caching.

Each submodule needs its own `git:` entry. LCS does not discover repositories from `.gitmodules`. The `chip-google-toolchain` target does not select Git repositories.

After you [start LCS and activate your shell](../../../README.md#activate), use your existing commands:

```bash
git clone https://github.com/project-chip/connectedhomeip.git
cd connectedhomeip
git submodule update --init --recursive
```

Activation appends `url.<base>.insteadOf` rules through `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_N`, and `GIT_CONFIG_VALUE_N`. Git sends matching requests to the loopback HTTP endpoint. Git caching does not need the LCS CA.

## CHIP on ESP32 and Linux

[examples/config.toml](../../../examples/config.toml) lists connectedhomeip, its larger ESP32 and Linux submodules, and larger nested dependencies.

These are ordinary `git:` entries. Merge the entries you need into your `cache` list. Keep `chip-google-toolchain` if you also want artifact caching. All entries share the same storage budget.

The direct submodule URLs match [connectedhomeip's Git metadata](https://github.com/project-chip/connectedhomeip/blob/4af1c9c61f39d4f6afd81370b3cdca2c5c95d63a/.gitmodules). Other revisions may use different URLs. Check your revision's `.gitmodules` files, including nested submodules, when updating the list.

## Cache behavior

LCS checks upstream refs for each new clone or fetch advertisement, including tags and HEAD. It compares those refs with the mirror's current refs before counting a hit. Concurrent requests share validation, and repository locks protect requests to `git http-backend`.

On a miss, LCS creates a mirror in staging. For a refresh, it copies the existing mirror and fetches changes there. The fetch includes objects referenced only by a detached HEAD.

Before publishing, LCS reads upstream refs again and sets the staged mirror's symbolic or detached HEAD. It compares the staged refs with that snapshot and checks the repository with Git. If the refs differ, LCS discards the staged mirror and sends the client upstream. Clients never read an incomplete mirror. Upstream can still change after validation.

Failed validation or a missing HEAD object causes LCS to bypass caching rather than serve old refs. LCS redirects GET and HEAD requests to the original server. It forwards POST requests without storing their responses.

Requests with authentication or cookies bypass caching. Anonymous mirror fetches do not read your credential helpers, Git configuration, or home-directory credentials.

Staging and refresh copies count against the shared budget. LCS samples staging growth and cancels a download that exceeds its reservation. Git can exceed the reservation before the next sample. Use a filesystem quota for a hard disk cap.

## Statistics

A hit means upstream refs matched the mirror. A miss means LCS needed to create or refresh it. Git transfers do not contribute to the traffic-saved counter. LCS does not measure the upstream pack transfer avoided by a hit.

## Tests and limits

Integration tests run Git against HTTP backends over TLS. They cover clone and fetch with protocol versions 0 and 2, tags, default-branch changes, and force pushes. They also cover shallow and partial clones, selected recursive submodules, and concurrent clones.

Freshness tests change and revert upstream refs during a download. They check detached HEAD commits that no branch references and transitions between detached and symbolic HEAD. Another test clears the mirror between a clone's requests and checks that the clone still completes.

Git LFS requests bypass caching. LFS and authenticated credential-helper workflows lack end-to-end tests, so full client compatibility remains unverified.
