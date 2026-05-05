package debuginfod

import (
	"context"
	"testing"
)

func TestClient_RejectsInvalidInput(t *testing.T) {
	client, err := NewClient(Options{ServerURLs: []string{"http://localhost"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("EmptyBuildID", func(t *testing.T) {
		if _, err := client.FetchDebugInfo(context.Background(), ""); err == nil {
			t.Error("expected error for empty build ID")
		}
	})

	t.Run("InvalidBuildID", func(t *testing.T) {
		for _, id := range []string{"not-hex", "../../etc/passwd", "aabb zzcc", "aabb\nccdd", "abc"} {
			if _, err := client.FetchDebugInfo(context.Background(), id); err == nil {
				t.Errorf("expected error for invalid build ID %q", id)
			}
		}
	})

	t.Run("EmptySourcePath", func(t *testing.T) {
		if _, err := client.FetchSource(context.Background(), "aabbccdd", ""); err == nil {
			t.Error("expected error for empty source path")
		}
	})

	t.Run("RelativeSourcePath", func(t *testing.T) {
		if _, err := client.FetchSource(context.Background(), "aabbccdd", "usr/src/main.c"); err == nil {
			t.Error("expected error for relative source path")
		}
	})

	t.Run("EmptySectionName", func(t *testing.T) {
		if _, err := client.FetchSection(context.Background(), "aabbccdd", ""); err == nil {
			t.Error("expected error for empty section name")
		}
	})
}
