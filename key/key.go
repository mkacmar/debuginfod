// Package key defines the Key type identifying a debuginfod artifact.
//
// A Key identifies a single artifact: a debug info file, an executable, a source file, or a section.
// Construct keys with DebugInfo, Executable, Source, or Section.
package key

import (
	"fmt"
	"strings"
)

// Kind identifies which debuginfod artifact a Key refers to.
type Kind int

const (
	KindDebugInfo Kind = iota + 1
	KindExecutable
	KindSource
	KindSection
)

func (k Kind) String() string {
	switch k {
	case KindDebugInfo:
		return "debuginfo"
	case KindExecutable:
		return "executable"
	case KindSource:
		return "source"
	case KindSection:
		return "section"
	}
	return fmt.Sprintf("unknown(%d)", int(k))
}

// Key uniquely identifies a debuginfod artifact.
// Build IDs are lowercased so keys constructed from differently-cased inputs compare equal.
type Key struct {
	kind      Kind
	buildID   string
	qualifier string
}

func DebugInfo(buildID string) Key {
	return Key{kind: KindDebugInfo, buildID: strings.ToLower(buildID)}
}

func Executable(buildID string) Key {
	return Key{kind: KindExecutable, buildID: strings.ToLower(buildID)}
}

// Source returns a Key identifying the source file at path for buildID.
// path must be the absolute source path as recorded in the debug info.
func Source(buildID, path string) Key {
	return Key{kind: KindSource, buildID: strings.ToLower(buildID), qualifier: path}
}

func Section(buildID, name string) Key {
	return Key{kind: KindSection, buildID: strings.ToLower(buildID), qualifier: name}
}

func (k Key) Kind() Kind { return k.kind }

func (k Key) BuildID() string { return k.buildID }

// Qualifier returns the kind-specific qualifier.
// For KindSource it is the absolute source path, for KindSection it is the section name.
// For KindDebugInfo and KindExecutable it is the empty string.
func (k Key) Qualifier() string { return k.qualifier }

// String returns a human-readable representation of k, suitable for logs and error messages.
func (k Key) String() string {
	if k.qualifier == "" {
		return fmt.Sprintf("%s/%s", k.buildID, k.kind)
	}
	return fmt.Sprintf("%s/%s/%s", k.buildID, k.kind, k.qualifier)
}

// Validate reports whether k is well-formed.
func (k Key) Validate() error {
	if k.buildID == "" {
		return fmt.Errorf("debuginfod/key: empty BuildID")
	}
	if len(k.buildID)%2 != 0 {
		return fmt.Errorf("debuginfod/key: invalid BuildID %q: odd length", k.buildID)
	}
	for i, r := range k.buildID {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("debuginfod/key: invalid BuildID %q: byte %d is not lowercase hex", k.buildID, i)
		}
	}
	switch k.kind {
	case KindDebugInfo, KindExecutable:
		if k.qualifier != "" {
			return fmt.Errorf("debuginfod/key: %s has unexpected qualifier", k)
		}
	case KindSource:
		if k.qualifier == "" {
			return fmt.Errorf("debuginfod/key: %s requires qualifier", k)
		}
		if !strings.HasPrefix(k.qualifier, "/") {
			return fmt.Errorf("debuginfod/key: %s source path must be absolute: %q", k, k.qualifier)
		}
		for _, seg := range strings.Split(k.qualifier[1:], "/") {
			if seg == "" || seg == "." || seg == ".." {
				return fmt.Errorf("debuginfod/key: %s source path has invalid segment %q", k, seg)
			}
		}
	case KindSection:
		if k.qualifier == "" {
			return fmt.Errorf("debuginfod/key: %s requires qualifier", k)
		}
		if k.qualifier == "." || k.qualifier == ".." {
			return fmt.Errorf("debuginfod/key: %s has invalid section name", k)
		}
		if strings.ContainsRune(k.qualifier, '\x00') {
			return fmt.Errorf("debuginfod/key: %s has invalid section name", k)
		}
	default:
		return fmt.Errorf("debuginfod/key: %s has unknown kind", k)
	}
	return nil
}
