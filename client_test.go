package debuginfod

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Client wrapper tests cover behavior added by Client.Fetch* on top of the underlying pipeline,
// namely build ID normalization and input validation wiring.
// Exhaustive validation cases live in key_test.go.

// TestClient_UppercaseBuildIDLowercased verifies the Client wrapper lowercases the build ID before dispatching to the pipeline.
// This is a Client-specific concern, the underlying Key.Validate requires lowercase.
func TestClient_UppercaseBuildIDLowercased(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, "data")
	}))
	defer srv.Close()

	client := mustNewClient(t, Options{ServerURLs: []string{srv.URL}})

	rc, err := client.FetchDebugInfo(context.Background(), strings.ToUpper(testBuildID))
	if err != nil {
		t.Fatal(err)
	}
	readAndClose(t, rc)

	if gotPath != "/buildid/"+testBuildID+"/debuginfo" {
		t.Errorf("expected lowercase path, got %q", gotPath)
	}
}

// TestClient_RejectsInvalidInput confirms each Fetch* wrapper calls Key.Validate before dispatching.
// One representative invalid input per wrapper is sufficient, key_test.go covers the validation matrix.
func TestClient_RejectsInvalidInput(t *testing.T) {
	client := mustNewClient(t, Options{ServerURLs: []string{"http://localhost"}})

	if _, err := client.FetchDebugInfo(context.Background(), ""); err == nil {
		t.Error("FetchDebugInfo: expected error for empty build ID")
	}
	if _, err := client.FetchSource(context.Background(), testBuildID, ""); err == nil {
		t.Error("FetchSource: expected error for empty source path")
	}
	if _, err := client.FetchSection(context.Background(), testBuildID, ""); err == nil {
		t.Error("FetchSection: expected error for empty section name")
	}
}
