# debuginfod

[![CI](https://github.com/mkacmar/debuginfod/actions/workflows/ci.yml/badge.svg)](https://github.com/mkacmar/debuginfod/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/go.kacmar.sk/debuginfod.svg)](https://pkg.go.dev/go.kacmar.sk/debuginfod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> **Note**: This is a v0 release, API may change.

A Go client library for [debuginfod](https://sourceware.org/elfutils/Debuginfod.html) servers.

Queries every configured server in parallel and returns the first authoritative response. Optionally caches artifacts on disk or in memory.

See [API documentation](https://pkg.go.dev/go.kacmar.sk/debuginfod) for details.

## Installation

```sh
go get go.kacmar.sk/debuginfod
```

Requires Go 1.25 or later.

## Usage

```go
client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
})
if err != nil {
    return err
}

rc, err := client.FetchDebugInfo(ctx, buildID)
if err != nil {
    return err
}
defer rc.Close()
```

All options besides `ServerURLs` are optional.
See [`Options`](https://pkg.go.dev/go.kacmar.sk/debuginfod#Options) for the full list of fields.

[`Client`](https://pkg.go.dev/go.kacmar.sk/debuginfod#Client) is safe for concurrent use.

Four artifact-specific methods are provided: [`FetchDebugInfo`](https://pkg.go.dev/go.kacmar.sk/debuginfod#Client.FetchDebugInfo), [`FetchExecutable`](https://pkg.go.dev/go.kacmar.sk/debuginfod#Client.FetchExecutable), [`FetchSource`](https://pkg.go.dev/go.kacmar.sk/debuginfod#Client.FetchSource), and [`FetchSection`](https://pkg.go.dev/go.kacmar.sk/debuginfod#Client.FetchSection).

### Error handling

If any server returns the artifact, it is returned immediately. Otherwise the library waits for every server to respond and reports one of:

- [`ErrNotFound`](https://pkg.go.dev/go.kacmar.sk/debuginfod#ErrNotFound), if at least one server returned 404 or 410. Treat as a definitive absence.
- [`ErrAuthRequired`](https://pkg.go.dev/go.kacmar.sk/debuginfod#ErrAuthRequired), if no server returned 404 or 410 and at least one returned 401 or 403. Treat as a hint to provide credentials.

5xx and 429 responses propagate as transport errors and are retried. All other 4xx codes are folded into `ErrNotFound`.

### Caching

Two cache implementations ship with the library:

- [`DiskCache`](https://pkg.go.dev/go.kacmar.sk/debuginfod#DiskCache), file-system-backed, persistent.
- [`MemoryCache`](https://pkg.go.dev/go.kacmar.sk/debuginfod#MemoryCache), in-process, useful for tests and short-lived programs.

```go
userCacheDir, err := os.UserCacheDir()
if err != nil {
    return err
}

cache, err := debuginfod.NewDiskCache(debuginfod.DiskCacheOptions{
    Dir: filepath.Join(userCacheDir, "debuginfod"),
})
if err != nil {
    return err
}
defer cache.Close()

client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
    Cache:      cache,
})
```

`DiskCache` does not bound its own size or evict old entries. Manage the cache directory externally (e.g. a periodic sweep based on file `mtime`), or implement a custom `Cache` with the eviction policy you need.

The returned `ReadCloser` streams bytes as they arrive from upstream and writes them into the cache. Closing it before `EOF` aborts the in-flight cache write so partial responses do not poison the cache.

[`Cache`](https://pkg.go.dev/go.kacmar.sk/debuginfod#Cache) is an interface. Callers can plug in alternative storage backends (e.g. shared blob store, ring buffer, content-addressed object store) by implementing `Fetch`, `Stage`, and `Evict`.
`Stage` returns a [`CacheEntry`](https://pkg.go.dev/go.kacmar.sk/debuginfod#CacheEntry) the library writes to and `Commit`s once the upstream response finishes.

See [`MemoryCache`](https://pkg.go.dev/go.kacmar.sk/debuginfod#MemoryCache) for a minimal reference implementation.

### Section requests

`FetchSection` calls the upstream `/section` endpoint and returns its response. 

Not every debuginfod server implements `/section`. When the endpoint is unavailable, `FetchSection` returns `ErrNotFound` and the client does not attempt any further action on its own.
The library never silently escalates a section request into a full debuginfo download. This is a policy decision left to the caller.

If you want a section and the upstream cannot serve it, fetch the full debuginfo and extract the section locally with [`debug/elf`](https://pkg.go.dev/debug/elf).

```go
func fetchSection(ctx context.Context, c *debuginfod.Client, buildID, name string) ([]byte, error) {
    rc, err := c.FetchSection(ctx, buildID, name)
    if err == nil {
        defer rc.Close()
        return io.ReadAll(rc)
    }
    if !errors.Is(err, debuginfod.ErrNotFound) {
        return nil, err
    }

    // /section unavailable, fall back to the full debuginfo and slice locally.
    rc, err = c.FetchDebugInfo(ctx, buildID)
    if err != nil {
        return nil, err
    }
    defer rc.Close()
    raw, err := io.ReadAll(rc)
    if err != nil {
        return nil, err
    }
    f, err := elf.NewFile(bytes.NewReader(raw))
    if err != nil {
        return nil, err
    }
    defer f.Close()
    s := f.Section(name)
    if s == nil {
        return nil, debuginfod.ErrNotFound
    }
    return io.ReadAll(s.Open())
}
```

### Retries

Each retry round fans out across all configured servers in parallel. Only network failures trigger retries. Any HTTP response from any server is authoritative and ends the round.

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

For private debuginfod instances, inject a custom `http.RoundTripper` via `HTTPOptions.Client` and decorate outbound requests with auth scheme you need. Per-host routing is straightforward via `req.URL.Host`. The same hook covers bearer tokens, basic auth, and mTLS (`Transport.TLSClientConfig`).

For shared or public servers, set `HTTPOptions.UserAgent` to identify your client to debuginfod operators.

### Logging

Pass a `*slog.Logger` via `Options.Logger` to surface non-fatal events such as retry attempts and cache write failures. When unset, the library logs nothing.

```go
client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
    Logger:     slog.Default(),
})
```

### Timeouts

The library does not impose timeouts of its own. 

Two knobs cover the common cases:

- **Total budget** across all servers and retries: set a deadline on the `context.Context` passed to `Fetch*` methods. On a cache miss, this budget covers the slowest server, not the fastest (see [Retries](#retries)).
- **Per-attempt budget**: configure your own `*http.Client` via `HTTPOptions.Client` and use `http.Client.Timeout`, `Transport.ResponseHeaderTimeout`, or a custom `net.Dialer.Timeout` (wired through `Transport.DialContext`) to bound how long any single server request may stall.

## License

MIT License - see [LICENSE](LICENSE) for details.
