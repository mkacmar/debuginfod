// Package debuginfod is a client for debuginfod federations.
//
// debuginfod is an HTTP service that distributes ELF debug artifacts keyed by GNU build ID.
// See https://sourceware.org/elfutils/Debuginfod.html for the protocol.
//
// Client queries every configured server in parallel and returns the first success.
// Network failures retry with exponential backoff.
// Any HTTP response is authoritative and short-circuits the retry loop.
//
// An optional Cache stores fetched artifacts.
// The provided DiskCache lays artifacts out per build ID so section requests can be sliced from cached debuginfo without going to the network.
//
// Client is safe for concurrent use.
package debuginfod
