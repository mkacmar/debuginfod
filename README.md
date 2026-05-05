# debuginfod

> **Note**: This is a v0 release, API may change.

A Go client library for [debuginfod](https://sourceware.org/elfutils/Debuginfod.html) servers.

Queries every configured server in parallel and returns the first success. Optionally caches artifacts on disk.

Section requests can be served by slicing cached debuginfo locally.

See [API documentation](https://pkg.go.dev/go.kacmar.sk/debuginfod) for details.

## Installation

```sh
go get go.kacmar.sk/debuginfod
```

## Usage

```go
client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
})

rc, err := client.FetchDebugInfo(ctx, buildID)
defer rc.Close()
```

`Client` is safe for concurrent use.

### Caching

```go
cacheDir, err := debuginfod.DefaultCacheDir()

cache, err := debuginfod.NewDiskCache(debuginfod.DiskCacheOptions{
    Dir: cacheDir,
})

client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
    Cache:      cache,
})
```

### Retries

Each retry round fans out across all configured servers in parallel. Only network failures trigger retries.

Any HTTP response from any server is authoritative.

```go
backoff, err := debuginfod.ExponentialBackoff(2*time.Second, 60*time.Second)

client, err := debuginfod.NewClient(debuginfod.Options{
    ServerURLs: []string{"https://debuginfod.elfutils.org"},
    HTTP: debuginfod.HTTPOptions{
        MaxRetries: 4,
        Backoff:    backoff,
    },
})
```

### Timeouts

The library does not impose timeouts of its own. Two knobs cover the common cases:

- **Total budget** across all servers and retries: set a deadline on the `context.Context` passed to `Fetch*` methods.
- **Per-attempt budget**: configure your own `*http.Client` and set fields like `Transport.ResponseHeaderTimeout` or `Transport.DialContext` to bound how long any single server request may stall.

## License

MIT License - see [LICENSE](LICENSE) for details.
