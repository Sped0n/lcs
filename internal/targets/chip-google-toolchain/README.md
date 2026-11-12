# Google toolchain target

`chip-google-toolchain` caches recognized CIPD downloads used by Pigweed and the CHIP toolchain. It does not select Git repositories or intercept GitHub or PyPI traffic. Select repositories through the separate [Git target](../git/README.md).

## Enable the target

Add the preset to `cache` in the [LCS configuration](../../../README.md#configure):

```toml
cache_size = "50GB"
cache = ["chip-google-toolchain"]
```

[Start LCS and activate your shell](../../../README.md#activate) before running your existing setup commands. HTTPS caching requires the certificate trust setup below. Without trust, HTTPS requests use tunnels without caching.

## Recognized downloads

The target recognizes these HTTPS URLs:

- The `/client` endpoint on `chrome-infra-packages.appspot.com`, with a version and a `linux-amd64` or `linux-arm64` platform.
- Paths under `/dl/` on that host that end in a CIPD instance ID and have no query.
- Paths under `/chrome-infra-packages/` on `storage.googleapis.com` that end in an instance ID and have no query.
- Exact URLs learned from public CIPD HTTP redirects to those hosts or a host under `googleusercontent.com`. These URLs can include signed queries.

LCS keeps learned redirects for ten minutes in memory. It does not treat an entire host as cacheable or retain redirect recognition across restarts.

Pigweed's CIPD JSON and RPC requests, arbitrary Google buckets, and blob URLs supplied only in RPC bodies pass through without caching.

## Certificate trust

The first server startup creates a local CA. With this target enabled, `lcsctl shell` checks whether the system trusts that CA.

If the CA is untrusted, the command prints a warning and the Ubuntu or Debian installation commands to stderr. It uses yellow text when stderr is a terminal. The commands include the resolved, shell-quoted certificate path. Stdout still contains valid shell statements.

Install the CA yourself if you want HTTPS caching. LCS never runs `sudo`, changes the certificate store, or disables certificate verification. After installation, activate the shell again:

```bash
eval "$(lcsctl shell)"
```

Activation uses `/etc/ssl/certs/ca-certificates.crt` for unset `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `SSL_CERT_FILE`, and `PIP_CERT` variables. This bundle must contain the normal system roots and the LCS CA. Activation preserves existing bundle variables. If a custom bundle does not trust LCS, activation disables interception.

LCS intercepts connections to the recognized Google dependency hosts and tunnels other destinations, including GitHub and PyPI. It verifies upstream TLS certificates. The credentials in the generated proxy URL mark the shell's choice to enable interception. They do not authenticate access to the proxy.

Tools that ignore proxy variables or use their own trust store may bypass caching or need a separate trust setup.

## Cache behavior

LCS stores complete HTTPS response bodies with a known, positive length. It checks the downloaded length and names each stored body by its SHA-256 hash. It verifies that hash before reuse and discards interrupted downloads.

Freshness follows `Cache-Control`, `Date`, `Age`, and `Expires`. Stale entries require upstream validation with an ETag or Last-Modified value before reuse. LCS separates request-header variants so responses with `Vary` do not cross variants.

LCS does not cache requests with authentication or cookies. It also excludes responses with a `Set-Cookie` header, `Vary: *`, or a `private` or `no-store` cache directive. Range requests, client conditional requests, request cache directives, and responses with an unknown length pass through unchanged.

Requests for the same artifact variant wait for an in-progress cache write. If the response cannot fit in the shared storage budget, LCS returns it without caching.

## Statistics

An artifact hit means LCS reused a stored body, including after successful upstream validation. A miss counts a recognized request that needed an upstream response. Traffic saved counts bytes read from cached bodies, not their full sizes when a reader stops early. The counter excludes network framing.

## Tests and limits

TLS integration tests cover cached responses, conditional validation, header variants, redirects, interrupted downloads, corrupt files, and storage limits. They also check that unselected downloads pass through without storage.

Full connectedhomeip and Pigweed bootstrap, live CIPD installation, and download tools with separate proxy or trust settings still need validation on a target machine. No cold-cache or warm-cache installation benchmark is available.
