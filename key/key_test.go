package key

import (
	"strings"
	"testing"
)

const testBuildID = "cafebabedeadbeef0123456789abcdef00112233"

func TestKind_String(t *testing.T) {
	tests := map[Kind]string{
		KindDebugInfo:  "debuginfo",
		KindExecutable: "executable",
		KindSource:     "source",
		KindSection:    "section",
	}
	for k, want := range tests {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(k), got, want)
		}
	}

	unknown := Kind(99).String()
	if !strings.Contains(unknown, "99") {
		t.Errorf("Kind(99).String() = %q, want to contain %q", unknown, "99")
	}
}

func TestKey_ConstructorsLowercaseBuildID(t *testing.T) {
	mixed := "AaBbCcDd"
	want := "aabbccdd"
	cases := []struct {
		name string
		key  Key
	}{
		{"DebugInfo", DebugInfo(mixed)},
		{"Executable", Executable(mixed)},
		{"Source", Source(mixed, "/x")},
		{"Section", Section(mixed, ".text")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.key.BuildID(); got != want {
				t.Errorf("BuildID() = %q, want %q", got, want)
			}
		})
	}
}

func TestKey_Accessors(t *testing.T) {
	cases := []struct {
		name      string
		key       Key
		wantKind  Kind
		wantBuild string
		wantQual  string
	}{
		{"DebugInfo", DebugInfo(testBuildID), KindDebugInfo, testBuildID, ""},
		{"Executable", Executable(testBuildID), KindExecutable, testBuildID, ""},
		{"Source", Source(testBuildID, "/usr/src/main.c"), KindSource, testBuildID, "/usr/src/main.c"},
		{"Section", Section(testBuildID, ".text"), KindSection, testBuildID, ".text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.key.Kind(); got != tc.wantKind {
				t.Errorf("Kind() = %v, want %v", got, tc.wantKind)
			}
			if got := tc.key.BuildID(); got != tc.wantBuild {
				t.Errorf("BuildID() = %q, want %q", got, tc.wantBuild)
			}
			if got := tc.key.Qualifier(); got != tc.wantQual {
				t.Errorf("Qualifier() = %q, want %q", got, tc.wantQual)
			}
		})
	}
}

func TestKey_Equality(t *testing.T) {
	if DebugInfo(strings.ToUpper(testBuildID)) != DebugInfo(testBuildID) {
		t.Error("differently-cased BuildIDs must produce equal keys")
	}
	if DebugInfo(testBuildID) == Executable(testBuildID) {
		t.Error("different kinds must produce unequal keys")
	}
	if Source(testBuildID, "/a") == Source(testBuildID, "/b") {
		t.Error("different qualifiers must produce unequal keys")
	}
}

func TestKey_MapKey(t *testing.T) {
	m := map[Key]string{
		DebugInfo(testBuildID):        "di",
		Executable(testBuildID):       "ex",
		Source(testBuildID, "/a"):     "src",
		Section(testBuildID, ".text"): "sec",
	}
	if got := m[DebugInfo(testBuildID)]; got != "di" {
		t.Errorf("map lookup = %q, want %q", got, "di")
	}
	if len(m) != 4 {
		t.Errorf("map length = %d, want 4", len(m))
	}
}

func TestKey_String(t *testing.T) {
	cases := []struct {
		key  Key
		want string
	}{
		{DebugInfo(testBuildID), testBuildID + "/debuginfo"},
		{Executable(testBuildID), testBuildID + "/executable"},
		{Source(testBuildID, "/usr/src/main.c"), testBuildID + "/source//usr/src/main.c"},
		{Section(testBuildID, ".text"), testBuildID + "/section/.text"},
	}
	for _, tc := range cases {
		if got := tc.key.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestKey_Validate(t *testing.T) {
	tests := []struct {
		name      string
		key       Key
		wantError bool
	}{
		{"DebugInfo", DebugInfo(testBuildID), false},
		{"Executable", Executable(testBuildID), false},
		{"SourceAbsolute", Source(testBuildID, "/usr/src/main.c"), false},
		{"SectionSimple", Section(testBuildID, ".text"), false},
		{"SectionWithSlash", Section(testBuildID, ".rela/.text"), false},

		{"ZeroValue", Key{}, true},
		{"EmptyBuildID", DebugInfo(""), true},
		{"OddBuildID", DebugInfo("abc"), true},
		{"NonHexBuildID", DebugInfo("aabbggdd"), true},
		{"BuildIDWithSpace", DebugInfo("aabb ccdd"), true},
		{"BuildIDWithNewline", DebugInfo("aabb\nccdd"), true},
		{"BuildIDWithSlash", DebugInfo("../../etc"), true},

		{"SourceEmptyQualifier", Source(testBuildID, ""), true},
		{"SourceRelative", Source(testBuildID, "usr/src/main.c"), true},
		{"SourceDotSegment", Source(testBuildID, "/a/./b"), true},
		{"SourceDotDotSegment", Source(testBuildID, "/a/../b"), true},
		{"SourceEmptySegment", Source(testBuildID, "/a//b"), true},
		{"SourceTrailingSlash", Source(testBuildID, "/a/b/"), true},
		{"SourceJustSlash", Source(testBuildID, "/"), true},

		{"SectionEmptyQualifier", Section(testBuildID, ""), true},
		{"SectionDot", Section(testBuildID, "."), true},
		{"SectionDotDot", Section(testBuildID, ".."), true},
		{"SectionNull", Section(testBuildID, ".text\x00"), true},
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
