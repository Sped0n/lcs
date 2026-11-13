# Git cache

LCS caches public HTTPS repositories selected by `git:<url>` entries. It uses bare repositories and the system `git http-backend` command. It does not cache SSH remotes.

## Select repositories

Add each repository to `cache` as `git:<url>` in the [LCS configuration](../../README.md#configure).

Use the same URL spelling as your Git commands, including the presence or absence of `.git`. Entries must use HTTPS on port 443, with no credentials, query, or fragment. Git uses its normal transport for remotes on other ports.

Each submodule needs its own entry. LCS does not discover repositories from `.gitmodules`.

After you [start LCS and activate your shell](../../README.md#activate), use your existing commands:

```bash
git clone https://github.com/project-chip/connectedhomeip.git
cd connectedhomeip
git submodule update --init --recursive
```

Activation adds `url.<base>.insteadOf` rules through `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_N`, and `GIT_CONFIG_VALUE_N`. Git sends matching requests to the loopback HTTP endpoint. LCS verifies upstream HTTPS certificates. The local endpoint needs no certificate.

## CHIP on ESP32 and Linux

[examples/config.toml](../../examples/config.toml) lists connectedhomeip, its larger ESP32 and Linux submodules, and larger nested dependencies.

Copy the entries you need into your `cache` list.

The direct submodule URLs match [connectedhomeip's Git metadata](https://github.com/project-chip/connectedhomeip/blob/4af1c9c61f39d4f6afd81370b3cdca2c5c95d63a/.gitmodules). Other revisions may use different URLs. Check your revision's `.gitmodules` files, including nested submodules, when updating the list.

## Cache behavior

LCS forwards ref-listing requests upstream, including protocol-v2 `ls-refs`. Listing refs does not download a mirror. LCS prepares a cache entry when the client requests objects.

Requests with authentication or cookies bypass the cache. LCS downloads cached repositories anonymously, without reading your credential helpers, Git configuration, or home-directory credentials.

### Depth-1 fetches

For an unfiltered depth-1 request, LCS fetches the requested object IDs into a shallow bare repository. This supports submodules pinned behind a branch tip without downloading the branch's full history. Git creates the shallow boundaries and handles negotiation.

When the client requests automatic tag following, LCS checks upstream tags and fetches matching tag objects. The cache key includes the requested and matching tag object IDs. A changed tag object therefore produces a different cache key. Concurrent identical requests share one download. Later requests reuse the completed repository.

LCS passes other depths, relative deepening, unshallow requests, and unrecognized negotiation options upstream unchanged. It does not try to expand a shallow entry into a full mirror.

### Full mirrors

An unfiltered clone without a depth limit populates a full mirror. If LCS has no full mirror, it forwards incremental or filtered fetches without caching. Existing full mirrors can serve those requests.

Before reusing a mirror, LCS compares its refs, tags, and HEAD with upstream. Concurrent requests share this check. LCS refreshes a separate copy of the mirror, including objects referenced only by a detached HEAD.

Before publishing, LCS reads upstream refs again and sets the copy's symbolic or detached HEAD. It compares resolved ref IDs and HEAD's symbolic target. Git can convert other symbolic aliases into direct refs in a mirror. LCS compares their object IDs to avoid treating this conversion as an upstream change.

If snapshots differ, LCS logs the differing refs and object IDs, then fetches incrementally into the same temporary repository. It makes at most three fetch and validation attempts. If they fail, LCS discards the temporary repository and forwards the client request upstream. LCS publishes only repositories that pass ref and object checks. Upstream can still change after validation.

### Storage and concurrency

LCS reserves space in chunks as Git downloads grow. It builds or refreshes at most four cached repositories at once. Clients can read cached entries in parallel. LCS keeps those entries until their readers finish.

Refreshes use hard links for immutable Git object files when the filesystem supports them. They copy mutable metadata. Each copy counts toward the shared budget by its logical size, even when hard links share disk blocks.

LCS checks temporary download sizes every 50 milliseconds. If it cannot reserve more space, it cancels the download and forwards the request upstream. Git can exceed a reservation between checks. See the [storage limits](../../README.md#storage).

Log messages identify cache downloads, completed entries, ref mismatches, and bypass errors. They do not include credentials.

## Statistics

A hit means an existing entry passed validation. A miss means LCS attempted to create or refresh an entry. Requests that share a download or validation check count once. Ref listings and unsupported requests do not count as hits or misses.

## Tests and limits

Integration tests use real Git processes and TLS upstreams. They cover protocols 0 and 2, clone and fetch, and selected recursive submodules. Ref tests cover tags, default-branch changes, force pushes, symbolic aliases, and detached HEAD.

Depth-1 tests fetch pinned submodules whose commits precede the branch tip. Their histories contain obsolete large blobs. The tests check upstream bytes and cache reuse, including after retagging. They also check relative deepening and unshallow fallback. Partial-clone tests check passthrough on a cold cache.

Other tests cover ref changes during validation, retry limits, concurrent readers and downloads, cancellation, capacity limits, and clear during a download.

Git LFS requests bypass the cache. LFS and authenticated credential-helper workflows have no end-to-end tests. Live connectedhomeip installation time and throughput have not been measured.
