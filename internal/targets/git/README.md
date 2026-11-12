# Git target

The Git target caches public HTTPS repositories selected by `git:<url>` entries. It uses bare mirrors and the system `git http-backend` command. SSH remotes do not pass through this target.

## Select repositories

Add each repository to `cache` in the [LCS configuration](../../../README.md#configure):

```toml
cache_size = "50GB"

cache = [
  "git:https://github.com/project-chip/connectedhomeip.git",
  "git:https://github.com/google/pigweed.git",
]
```

Use the same URL spelling in Git commands, including the `.git` suffix. URLs must use HTTPS and must not contain credentials, a query, or a fragment.

Each submodule repository needs its own `git:` entry. LCS does not discover or select more repositories from `.gitmodules`. The `chip-google-toolchain` preset does not select Git repositories.

After you [start LCS and activate your shell](../../../README.md#activate), use your existing commands:

```bash
git clone https://github.com/project-chip/connectedhomeip.git
cd connectedhomeip
git submodule update --init --recursive
```

Activation appends repository-specific `url.<base>.insteadOf` rules through `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_N`, and `GIT_CONFIG_VALUE_N`. Git sends matching requests to the loopback HTTP endpoint. Git caching does not need the LCS CA.

## Cache behavior

LCS checks upstream refs for each new clone or fetch advertisement. The check includes tags and HEAD. A cache hit requires the mirror's actual refs to match upstream, not just its stored validation hash. Concurrent requests share validation, and repository locks protect requests to `git http-backend`.

On a miss, LCS creates a mirror in staging. To refresh a mirror, it copies the existing mirror to staging and fetches changes there, including objects referenced only by a detached HEAD.

Before publishing, LCS reads upstream refs again and applies that snapshot's symbolic or detached HEAD. It compares the staged mirror's actual refs with the new snapshot and checks the repository with Git. A mismatch prevents publication and sends the client upstream. Clients never read an incomplete mirror. Upstream can still change after validation.

If upstream validation fails or upstream does not advertise a valid HEAD object, LCS bypasses caching. It does not serve old refs as current. LCS redirects GET and HEAD requests to the original server and forwards POST requests without storing their responses.

Requests with authentication or cookies bypass caching. Anonymous mirror fetches do not read your credential helpers, Git configuration, or home-directory credentials. LCS never caches Git Smart HTTP POST responses as HTTP objects.

Staging files and refresh copies count against the shared storage budget. LCS samples Git's staging growth during downloads and cancels a download if it exceeds its reservation. A Git subprocess can exceed the reservation before the next sample. This check does not replace a filesystem quota.

## Statistics

A Git hit means the upstream refs matched the cached mirror. A miss means LCS needed to create or refresh the mirror. LCS excludes Git from the traffic-saved counter because it does not measure the upstream pack transfer that a cache hit avoids.

## Tests and limits

Integration tests run Git against HTTP backends over TLS. They cover protocol versions 0 and 2, clone and fetch, tags, default-branch changes, force pushes, shallow and partial clones, selected recursive submodules, and concurrent clones. Freshness regressions cover an upstream change followed by a reversal during download, detached HEAD commits that no branch references, and transitions between detached and symbolic HEAD. A test also checks that a clone completes when clear removes its mirror between requests.

Git LFS requests bypass caching, but LFS and authenticated credential-helper workflows do not have end-to-end integration coverage. Full client compatibility for those workflows remains unverified.
