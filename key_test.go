package debuginfod

import (
	"strings"
	"testing"
)

func TestKey_Validate(t *testing.T) {
	tests := []struct {
		name      string
		key       Key
		wantError bool
	}{
		{"DebugInfo", Key{BuildID: "aabbccdd", Kind: KindDebugInfo}, false},
		{"Executable", Key{BuildID: "aabbccdd", Kind: KindExecutable}, false},
		{"SourceAbsolute", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/usr/src/main.c"}, false},
		{"SectionSimple", Key{BuildID: "aabbccdd", Kind: KindSection, Qualifier: ".text"}, false},
		{"SectionWithSlash", Key{BuildID: "aabbccdd", Kind: KindSection, Qualifier: ".rela/.text"}, false},

		{"EmptyBuildID", Key{Kind: KindDebugInfo}, true},
		{"UpperHexBuildID", Key{BuildID: "AABBCCDD", Kind: KindDebugInfo}, true},
		{"MixedHexBuildID", Key{BuildID: "aaBBccDD", Kind: KindDebugInfo}, true},
		{"OddBuildID", Key{BuildID: "abc", Kind: KindDebugInfo}, true},
		{"NonHexBuildID", Key{BuildID: "aabbggdd", Kind: KindDebugInfo}, true},
		{"BuildIDWithSpace", Key{BuildID: "aabb ccdd", Kind: KindDebugInfo}, true},
		{"BuildIDWithNewline", Key{BuildID: "aabb\nccdd", Kind: KindDebugInfo}, true},
		{"BuildIDWithSlash", Key{BuildID: "../../etc", Kind: KindDebugInfo}, true},

		{"DebugInfoWithQualifier", Key{BuildID: "aabbccdd", Kind: KindDebugInfo, Qualifier: "extra"}, true},
		{"ExecutableWithQualifier", Key{BuildID: "aabbccdd", Kind: KindExecutable, Qualifier: "extra"}, true},

		{"SourceEmptyQualifier", Key{BuildID: "aabbccdd", Kind: KindSource}, true},
		{"SourceRelative", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "usr/src/main.c"}, true},
		{"SourceDotSegment", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/a/./b"}, true},
		{"SourceDotDotSegment", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/a/../b"}, true},
		{"SourceEmptySegment", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/a//b"}, true},
		{"SourceTrailingSlash", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/a/b/"}, true},
		{"SourceJustSlash", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/"}, true},

		{"SectionEmptyQualifier", Key{BuildID: "aabbccdd", Kind: KindSection}, true},
		{"SectionDot", Key{BuildID: "aabbccdd", Kind: KindSection, Qualifier: "."}, true},
		{"SectionDotDot", Key{BuildID: "aabbccdd", Kind: KindSection, Qualifier: ".."}, true},
		{"SectionNull", Key{BuildID: "aabbccdd", Kind: KindSection, Qualifier: ".text\x00"}, true},

		{"UnknownKind", Key{BuildID: "aabbccdd", Kind: ArtifactKind(99)}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.key.Validate()
			if tc.wantError && err == nil {
				t.Errorf("Validate() = nil, want error")
			}
			if !tc.wantError && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestKey_URLPath(t *testing.T) {
	tests := []struct {
		name string
		key  Key
		want string
	}{
		{"DebugInfo", Key{BuildID: "aabbccdd", Kind: KindDebugInfo}, "/buildid/aabbccdd/debuginfo"},
		{"Executable", Key{BuildID: "aabbccdd", Kind: KindExecutable}, "/buildid/aabbccdd/executable"},
		{"Source", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/usr/src/main.c"}, "/buildid/aabbccdd/source/usr/src/main.c"},
		{"SourcePreservesSlashes", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/a/b/c.c"}, "/buildid/aabbccdd/source/a/b/c.c"},
		{"SourceEscapesSegment", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/dir with space/x.c"}, "/buildid/aabbccdd/source/dir%20with%20space/x.c"},
		{"SourceEscapesPercent", Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/a/100%foo.c"}, "/buildid/aabbccdd/source/a/100%25foo.c"},
		{"Section", Key{BuildID: "aabbccdd", Kind: KindSection, Qualifier: ".text"}, "/buildid/aabbccdd/section/.text"},
		{"SectionEscapesSlash", Key{BuildID: "aabbccdd", Kind: KindSection, Qualifier: ".rela/.text"}, "/buildid/aabbccdd/section/.rela%2F.text"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.key.URLPath()
			if got != tc.want {
				t.Errorf("URLPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKey_String(t *testing.T) {
	tests := []struct {
		key  Key
		want string
	}{
		{Key{BuildID: "aabbccdd", Kind: KindDebugInfo}, "aabbccdd/debuginfo"},
		{Key{BuildID: "aabbccdd", Kind: KindSource, Qualifier: "/usr/src/main.c"}, "aabbccdd/source//usr/src/main.c"},
		{Key{BuildID: "aabbccdd", Kind: KindSection, Qualifier: ".text"}, "aabbccdd/section/.text"},
	}
	for _, tc := range tests {
		got := tc.key.String()
		if got != tc.want {
			t.Errorf("Key{%s,%s,%q}.String() = %q, want %q", tc.key.BuildID, tc.key.Kind, tc.key.Qualifier, got, tc.want)
		}
	}
}

func TestArtifactKind_String(t *testing.T) {
	tests := map[ArtifactKind]string{
		KindDebugInfo:  "debuginfo",
		KindExecutable: "executable",
		KindSource:     "source",
		KindSection:    "section",
	}
	for k, want := range tests {
		if got := k.String(); got != want {
			t.Errorf("ArtifactKind(%d).String() = %q, want %q", int(k), got, want)
		}
	}

	unknown := ArtifactKind(99).String()
	if !strings.Contains(unknown, "99") {
		t.Errorf("ArtifactKind(99).String() = %q, want to contain %q", unknown, "99")
	}
}
