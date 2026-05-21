// Package debuginfod is a transport client for the debuginfod protocol.
//
// debuginfod is an HTTP service that distributes ELF debug artifacts keyed by GNU build ID.
// See https://sourceware.org/elfutils/Debuginfod.html for the protocol.
//
// For random-access reads or on-disk caching, use *cache.DiskCache from the debuginfod/cache subpackage.
package debuginfod
