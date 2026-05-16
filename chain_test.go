package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestChain_Fetch(t *testing.T) {
	cases := []struct {
		name      string
		firstFn   func(context.Context, Key) (io.ReadCloser, error)
		wantCalls [2]int
	}{
		{"FirstSourceHits_SecondNotCalled", stubBytes(testPayload), [2]int{1, 0}},
		{"FallsThroughOnErrorToSecond", stubError(ErrNotFound), [2]int{1, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := newStubSource(tc.firstFn)
			second := newStubSource(stubBytes(testPayload))

			c := newChain([]source{first, second}, discardLogger())
			rc, err := c.Fetch(context.Background(), testKey)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()

			if !bytes.Equal(got, testPayload) {
				t.Errorf("body = %q, want %q", got, testPayload)
			}
			if first.callCount() != tc.wantCalls[0] || second.callCount() != tc.wantCalls[1] {
				t.Errorf("calls = (%d, %d), want %v", first.callCount(), second.callCount(), tc.wantCalls)
			}
		})
	}
}

func TestChain_PanicsOnEmptySources(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("newChain with empty sources should panic")
		}
	}()
	newChain(nil, discardLogger())
}

func TestChain_ReturnsLastErrorWhenAllFail(t *testing.T) {
	firstErr := errors.New("first failure")
	lastErr := errors.New("last failure")
	first := newStubSource(stubError(firstErr))
	last := newStubSource(stubError(lastErr))

	c := newChain([]source{first, last}, discardLogger())
	_, err := c.Fetch(context.Background(), testKey)
	if !errors.Is(err, lastErr) {
		t.Errorf("err = %v, want last source error %v", err, lastErr)
	}
	if errors.Is(err, firstErr) {
		t.Errorf("err unexpectedly wraps first source error: %v", err)
	}
}
