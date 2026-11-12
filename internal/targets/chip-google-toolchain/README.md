# Google toolchain target

`chip-google-toolchain` caches recognized CIPD downloads used by Pigweed and the CHIP toolchain. It does not select Git repositories or intercept GitHub or PyPI traffic. Select repositories through the [Git target](../git/README.md).

## Enable the target

Add `chip-google-toolchain` to `cache` in the [LCS configuration](../../../README.md#configure):

```toml
cache_size = "50GB"
cache = ["chip-google-toolchain"]
```

[Start LCS and activate your shell](../../../README.md#activate) before running setup commands. HTTPS caching requires the trust setup below. Without it, HTTPS requests use tunnels without caching.

## Recognized downloads

The target recognizes these HTTPS URLs:

- `/client` on `chrome-infra-packages.appspot.com`, with a version and a `linux-amd64` or `linux-arm64` platform.
- Paths under `/dl/` on that host that end in a CIPD instance ID and have no query.
- Paths under `/chrome-infra-packages/` on `storage.googleapis.com` that end in an instance ID and have no query.
- Exact URLs learned from public CIPD HTTP redirects to those hosts or a host under `googleusercontent.com`. These URLs can include signed queries.

LCS keeps learned redirect URLs in memory for ten minutes. They do not survive a restart, and they do not make the rest of a host cacheable.

CIPD JSON and RPC requests, arbitrary Google buckets, and blob URLs supplied only in RPC bodies pass through without caching.

## Certificate trust

The first server startup creates a local CA. With this target enabled, `lcsctl shell` checks whether the system trusts that CA.

If the CA is untrusted, the command prints a warning and Ubuntu/Debian installation commands to stderr. The warning is yellow when stderr is a terminal. The commands use the resolved, shell-quoted certificate path. Stdout still contains executable shell statements.

Install the CA yourself to enable HTTPS caching. LCS never runs `sudo`, changes the certificate store, or disables certificate verification. After installation, activate the shell again:

```bash
eval "$(lcsctl shell)"
```

For unset `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `SSL_CERT_FILE`, and `PIP_CERT` variables, activation uses `/etc/ssl/certs/ca-certificates.crt`. This bundle must contain the normal system roots and the LCS CA. Existing bundle variables stay unchanged. A custom bundle that does not trust LCS disables interception.

LCS intercepts port-443 connections to recognized Google dependency hosts. It tunnels other destinations, including custom ports, GitHub, and PyPI. It verifies upstream TLS certificates. Credentials in the generated proxy URL tell LCS to enable interception. They do not authenticate access to the proxy.

Tools that ignore proxy variables bypass LCS. Tools with their own trust store may need a separate trust setup before interception works.

## Cache behavior

LCS stores complete HTTPS bodies with a known, positive length. It checks the downloaded length and names the stored file by its SHA-256 hash. It checks that hash before reuse and discards interrupted downloads.

`Cache-Control`, `Date`, `Age`, and `Expires` determine when LCS can reuse a response. After expiry, LCS revalidates with an ETag or Last-Modified value when available. Otherwise, it fetches a new body. Requests with different headers do not share cached bodies, so `Vary` responses do not cross variants.

Requests with authentication or cookies bypass caching. LCS also excludes responses with `Set-Cookie`, `Vary: *`, or a `private` or `no-store` cache directive. Range requests, client conditional requests, request cache directives, and responses with an unknown length pass through unchanged.

Requests for the same artifact variant wait for an in-progress cache write. If the response cannot fit in the shared budget, LCS returns it without caching.

## Statistics

A hit means LCS reused a stored body, including after upstream validation. A miss means a recognized request needed an upstream response. Traffic saved counts bytes read from cached bodies, not unread bytes or network framing.

## Tests and limits

TLS integration tests cover reuse, conditional validation, header variants, redirects, interrupted downloads, corrupt files, and storage limits. They also check that unselected downloads pass through without storage.

Full connectedhomeip and Pigweed bootstrap and live CIPD installation remain unverified. Tools with separate proxy or trust settings also need validation on a target machine. No cold-cache or warm-cache installation benchmark is available.
