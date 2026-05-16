package debuginfod

import (
	"fmt"
	"net/url"
	"strings"
)

// ArtifactKind identifies which debuginfod artifact a Key refers to.
type ArtifactKind int

const (
	KindDebugInfo ArtifactKind = iota
	KindExecutable
	KindSource
	KindSection
)

// String returns the protocol verb for k, e.g. "debuginfo" or "section".
// These values are part of the debuginfod wire protocol and appear as URL path segments.
func (k ArtifactKind) String() string {
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
//
// BuildID is the GNU build ID as lowercase hex, matching the debuginfod wire protocol.
// Qualifier is the source path for KindSource, the section name for KindSection, and empty otherwise.
type Key struct {
	BuildID   string
	Kind      ArtifactKind
	Qualifier string
}

func (k Key) String() string {
	if k.Qualifier == "" {
		return fmt.Sprintf("%s/%s", k.BuildID, k.Kind)
	}
	return fmt.Sprintf("%s/%s/%s", k.BuildID, k.Kind, k.Qualifier)
}

// Validate reports whether k is well-formed.
//
// Sources may assume keys passed to them have been validated.
// The Fetch* convenience methods on Client validate before dispatching to the underlying pipeline.
func (k Key) Validate() error {
	if k.BuildID == "" {
		return fmt.Errorf("debuginfod: key has empty BuildID")
	}
	if len(k.BuildID)%2 != 0 {
		return fmt.Errorf("debuginfod: key has invalid BuildID %q: odd length", k.BuildID)
	}
	for i, r := range k.BuildID {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("debuginfod: key has invalid BuildID %q: byte %d is not lowercase hex", k.BuildID, i)
		}
	}
	switch k.Kind {
	case KindDebugInfo, KindExecutable:
		if k.Qualifier != "" {
			return fmt.Errorf("debuginfod: key %s has unexpected qualifier", k)
		}
	case KindSource:
		if k.Qualifier == "" {
			return fmt.Errorf("debuginfod: key %s requires qualifier", k)
		}
		if !strings.HasPrefix(k.Qualifier, "/") {
			return fmt.Errorf("debuginfod: key %s source path must be absolute: %q", k, k.Qualifier)
		}
		for _, seg := range strings.Split(k.Qualifier[1:], "/") {
			if seg == "" || seg == "." || seg == ".." {
				return fmt.Errorf("debuginfod: key %s source path has invalid segment %q", k, seg)
			}
		}
	case KindSection:
		if k.Qualifier == "" {
			return fmt.Errorf("debuginfod: key %s requires qualifier", k)
		}
		if k.Qualifier == "." || k.Qualifier == ".." {
			// Reject names that would collide with directory entries when used as filesystem paths by Cache implementations.
			return fmt.Errorf("debuginfod: key %s has invalid section name", k)
		}
		if strings.ContainsRune(k.Qualifier, '\x00') {
			return fmt.Errorf("debuginfod: key %s has invalid section name", k)
		}
	default:
		return fmt.Errorf("debuginfod: key %s has unknown kind", k)
	}
	return nil
}

// URLPath returns the debuginfod URL path for k, relative to a server's base URL.
//
// The returned path always begins with "/buildid/" and is suitable for appending after a debuginfod server's base URL.
// URLPath assumes k is valid, callers should call Validate first.
func (k Key) URLPath() string {
	var b strings.Builder
	b.WriteString("/buildid/")
	b.WriteString(k.BuildID)
	b.WriteByte('/')
	b.WriteString(k.Kind.String())
	switch k.Kind {
	case KindSource:
		b.WriteString(escapeSourcePath(k.Qualifier))
	case KindSection:
		b.WriteByte('/')
		b.WriteString(url.PathEscape(k.Qualifier))
	}
	return b.String()
}

// escapeSourcePath URL-escapes each "/"-separated segment, preserving the separators.
func escapeSourcePath(path string) string {
	parts := strings.Split(path, "/")
	for i, segment := range parts {
		parts[i] = url.PathEscape(segment)
	}
	return strings.Join(parts, "/")
}
