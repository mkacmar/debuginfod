// Package debuginfod is a client for the debuginfod protocol.
//
// debuginfod is an HTTP service that distributes ELF debug artifacts keyed by GNU build ID.
// See https://sourceware.org/elfutils/Debuginfod.html for the protocol.
//
// Client queries one or more upstream debuginfod servers, optionally backed by a Cache.
// When a Cache is configured, it is consulted first, upstream servers are queried in parallel on miss,
// and successful responses are written through to the cache.
// The library is transport-only and does not parse artifacts.
//
// Cache is the extension point for storage backends.
// DiskCache and MemoryCache are provided as ready-to-use implementations.
package debuginfod
