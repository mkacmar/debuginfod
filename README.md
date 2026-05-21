# debuginfod

[![CI](https://github.com/mkacmar/debuginfod/actions/workflows/ci.yml/badge.svg)](https://github.com/mkacmar/debuginfod/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/go.kacmar.sk/debuginfod.svg)](https://pkg.go.dev/go.kacmar.sk/debuginfod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> **Note**: This is a v0 release, API may change.

A Go client library for [debuginfod](https://sourceware.org/elfutils/Debuginfod.html) servers.

Queries every configured server in parallel and returns the first authoritative response. An optional companion package provides on-disk caching.

See [API documentation](https://pkg.go.dev/go.kacmar.sk/debuginfod) for details.

## Installation

```sh
go get go.kacmar.sk/debuginfod
```

Requires Go 1.25 or later.

## Usage

```go
import (
    "go.kacmar.sk/debuginfod"
    "go.kacmar.sk/debuginfod/key"
)

client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
})
if err != nil {
    return err
}

rc, err := client.Fetch(ctx, key.DebugInfo(buildID))
if err != nil {
    return err
}
defer rc.Close()
```

The [`key`](https://pkg.go.dev/go.kacmar.sk/debuginfod/key) package exposes constructors for the four artifact kinds: `DebugInfo`, `Executable`, `Source`, and `Section`. All options besides `ServerURLs` are optional. See [`Options`](https://pkg.go.dev/go.kacmar.sk/debuginfod#Options) for the full list.

### Error handling

If any server returns the artifact, it is returned immediately. Otherwise the library waits for every server to respond and reports one of:

- [`ErrNotFound`](https://pkg.go.dev/go.kacmar.sk/debuginfod#ErrNotFound), if at least one server returned 404 or 410. Treat as a definitive absence.
- [`ErrAuthRequired`](https://pkg.go.dev/go.kacmar.sk/debuginfod#ErrAuthRequired), if no server returned 404 or 410 and at least one returned 401 or 403. Treat as a hint to provide credentials.

5xx and 429 responses propagate as transport errors and are retried. All other 4xx codes are folded into `ErrNotFound`.

### Caching

The [`cache`](https://pkg.go.dev/go.kacmar.sk/debuginfod/cache) subpackage provides [`DiskCache`](https://pkg.go.dev/go.kacmar.sk/debuginfod/cache#DiskCache), a filesystem-backed cache that wraps a `Client` (or any value implementing the package's `Fetcher` interface) and returns `*os.File` handles suitable for random-access reads, for example ELF parsing via [`debug/elf`](https://pkg.go.dev/debug/elf).

```go
import (
    "go.kacmar.sk/debuginfod"
    "go.kacmar.sk/debuginfod/cache"
    "go.kacmar.sk/debuginfod/key"
)

userCacheDir, err := os.UserCacheDir()
if err != nil {
    return err
}

client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
})
if err != nil {
    return err
}

disk, err := cache.NewDiskCache(cache.DiskCacheOptions{
    Client: client,
    Dir:    filepath.Join(userCacheDir, "debuginfod"),
})
if err != nil {
    return err
}
defer disk.Close()

f, err := disk.Get(ctx, key.DebugInfo(buildID))
if err != nil {
    return err
}
defer f.Close()
```

Writes commit atomically via a staging file. The cache is symlink-safe and refuses to follow symlinks pointing outside the cache directory. `Get` resolves cache hits without invoking the underlying `Client`. On a miss, the fetched response is streamed to disk and the committed file is returned.

`DiskCache` does not bound its own size or delete old entries automatically. Use `Delete` to remove a single entry, or manage the cache directory externally (e.g. a periodic sweep based on file `mtime`).

`Get` does not coalesce concurrent requests for the same uncached key. All callers will fetch. Misses are not cached, so every `Get` for an absent artifact re-runs the federated lookup.

### Section requests

`Fetch` with a `key.Section` calls the upstream `/section` endpoint and returns its response. Not every debuginfod server implements `/section`, in which case the call returns `ErrNotFound` and the library does not attempt any further action on its own.
The library never silently escalates a section request into a full debuginfo download, which is a policy decision left to the caller.

If you want a section and the upstream cannot serve it, fetch the full debuginfo and extract the section locally with [`debug/elf`](https://pkg.go.dev/debug/elf). The `cache.DiskCache` is well-suited to this pattern since it returns `*os.File`.

### Retries

Each retry round fans out across all configured servers in parallel. A body or an authoritative error (`ErrNotFound` or `ErrAuthRequired`) from any server ends the round. Transport errors (network failures plus 5xx and 429 responses) trigger another round.

A cache miss requires every configured server to respond, since one server's 404 is not authoritative for the federation. Total latency on a miss is therefore bounded by the slowest server, not the fastest.

Use a context deadline or a per-attempt HTTP transport timeout to cap this (see [Timeouts](#timeouts)).

```go
backoff, err := debuginfod.ExponentialBackoff(2*time.Second, 60*time.Second)
if err != nil {
    return err
}

client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
    HTTP: debuginfod.HTTPOptions{
        MaxRetries: 4,
        Backoff:    backoff,
    },
})
```

When `Backoff` is unset, the library uses [`ExponentialBackoff`](https://pkg.go.dev/go.kacmar.sk/debuginfod#ExponentialBackoff)`(1s, 30s)` (Full Jitter, doubling each round, capped at the max).

### Authentication and custom headers

For private debuginfod instances, inject a custom `http.RoundTripper` via `HTTPOptions.Client` and decorate outbound requests with the auth scheme you need (per-host routing via `req.URL.Host`). The same hook covers bearer tokens, basic auth, and mTLS (`Transport.TLSClientConfig`).

For shared or public servers, set `HTTPOptions.UserAgent` to identify your client to debuginfod operators.

### Logging

Pass a `*slog.Logger` via `Options.Logger` to surface non-fatal events such as retry attempts. When unset, the library logs nothing.

```go
client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
    Logger:     slog.Default(),
})
```

### Timeouts

The library does not impose timeouts of its own.

Two knobs cover the common cases:

- **Total budget** across all servers and retries: set a deadline on the `context.Context` passed to `Fetch`. On a cache miss, this budget covers the slowest server, not the fastest (see [Retries](#retries)).
- **Per-attempt budget**: configure your own `*http.Client` via `HTTPOptions.Client` and use `http.Client.Timeout`, `Transport.ResponseHeaderTimeout`, or a custom `net.Dialer.Timeout` (wired through `Transport.DialContext`) to bound how long any single server request may stall.

## License

MIT License - see [LICENSE](LICENSE) for details.
