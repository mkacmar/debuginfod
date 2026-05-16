package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// expectEntryClose asserts that the next cache entry has finished its lifecycle and returns whether it was committed.
func expectEntryClose(t *testing.T, cache *signalingCache) bool {
	t.Helper()
	select {
	case committed := <-cache.closed:
		return committed
	default:
		t.Fatal("cache entry was not closed")
		return false
	}
}

func TestTeeing_WritesThroughOnSuccess(t *testing.T) {
	inner := newStubSource(stubBytes(testPayload))
	cache := newSignalingCache(NewMemoryCache())

	tee := newTeeing(inner, cache, discardLogger())
	rc, err := tee.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	rc.Close()

	if !bytes.Equal(got, testPayload) {
		t.Errorf("body = %q, want %q", got, testPayload)
	}
	if committed := expectEntryClose(t, cache); !committed {
		t.Error("cache entry not committed after successful read")
	}

	cached, err := cache.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("cache Fetch: %v", err)
	}
	cachedBytes, _ := io.ReadAll(cached)
	cached.Close()
	if !bytes.Equal(cachedBytes, testPayload) {
		t.Errorf("cached = %q, want %q", cachedBytes, testPayload)
	}
}

func TestTeeing_PropagatesInnerError(t *testing.T) {
	innerErr := errors.New("upstream failure")
	inner := newStubSource(stubError(innerErr))
	cache := NewMemoryCache()

	tee := newTeeing(inner, cache, discardLogger())
	_, err := tee.Fetch(context.Background(), testKey)
	if !errors.Is(err, innerErr) {
		t.Errorf("err = %v, want %v", err, innerErr)
	}

	if _, err := cache.Fetch(context.Background(), testKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("cache populated despite inner error: %v", err)
	}
}

func TestTeeing_StageFailureStillServesCaller(t *testing.T) {
	inner := newStubSource(stubBytes(testPayload))
	cache := &failingStageCache{MemoryCache: *NewMemoryCache()}

	tee := newTeeing(inner, cache, discardLogger())
	rc, err := tee.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()

	if !bytes.Equal(got, testPayload) {
		t.Errorf("body = %q, want %q (caller must still receive full payload)", got, testPayload)
	}
}

func TestTeeing_CommitFailureStillServesCaller(t *testing.T) {
	inner := newStubSource(stubBytes(testPayload))
	cache := newSignalingCache(&failingCommitCache{MemoryCache: *NewMemoryCache()})

	tee := newTeeing(inner, cache, discardLogger())
	rc, err := tee.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()

	if !bytes.Equal(got, testPayload) {
		t.Errorf("body = %q, want %q", got, testPayload)
	}
	if committed := expectEntryClose(t, cache); committed {
		t.Error("entry reported committed despite commit failure")
	}
}

func TestTeeing_CacheWriteFailureStillServesCaller(t *testing.T) {
	payload := bytes.Repeat([]byte("X"), 4096)
	inner := newStubSource(stubBytes(payload))
	cache := newSignalingCache(&failingWriteCache{MemoryCache: *NewMemoryCache(), failAfter: 100})

	tee := newTeeing(inner, cache, discardLogger())
	rc, err := tee.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, readErr := io.ReadAll(rc)
	rc.Close()

	if readErr != nil {
		t.Errorf("caller read err = %v, want nil (cache failure must not affect caller)", readErr)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("body len = %d, want %d", len(got), len(payload))
	}
	if committed := expectEntryClose(t, cache); committed {
		t.Error("cache entry committed despite write failure")
	}
	if _, err := cache.Fetch(context.Background(), testKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("cache populated despite write failure: %v", err)
	}
}

// errorReader emits data once and then errors on the next Read.
type errorReader struct {
	data []byte
	err  error
	read bool
}

func (r *errorReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		n := copy(p, r.data)
		return n, nil
	}
	return 0, r.err
}

func (r *errorReader) Close() error { return nil }

func TestTeeing_BodyErrorAbortsCache(t *testing.T) {
	bodyErr := errors.New("body read failure")
	inner := newStubSource(func(context.Context, Key) (io.ReadCloser, error) {
		return &errorReader{data: testPayload, err: bodyErr}, nil
	})
	cache := newSignalingCache(NewMemoryCache())

	tee := newTeeing(inner, cache, discardLogger())
	rc, err := tee.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	_, readErr := io.ReadAll(rc)
	rc.Close()

	if !errors.Is(readErr, bodyErr) {
		t.Errorf("caller read err = %v, want %v", readErr, bodyErr)
	}
	if committed := expectEntryClose(t, cache); committed {
		t.Error("cache entry committed despite body error")
	}
	if _, err := cache.Fetch(context.Background(), testKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("cache populated despite body error: %v", err)
	}
}

// blockingReader serves data once, then blocks on Read until Close is called.
// After Close it reports an error to mimic an interrupted upstream body.
type blockingReader struct {
	data   []byte
	served bool
	closed chan struct{}
}

func newBlockingReader(data []byte) *blockingReader {
	return &blockingReader{data: data, closed: make(chan struct{})}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.served {
		r.served = true
		return copy(p, r.data), nil
	}
	<-r.closed
	return 0, errors.New("body closed mid-stream")
}

func (r *blockingReader) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}

func TestTeeing_EarlyCallerCloseDiscardsEntry(t *testing.T) {
	body := newBlockingReader(testPayload)
	inner := newStubSource(func(context.Context, Key) (io.ReadCloser, error) {
		return body, nil
	})
	cache := newSignalingCache(NewMemoryCache())

	tee := newTeeing(inner, cache, discardLogger())
	rc, err := tee.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	buf := make([]byte, len(testPayload))
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if committed := expectEntryClose(t, cache); committed {
		t.Error("cache entry committed despite early caller close")
	}
	if _, err := cache.Fetch(context.Background(), testKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("cache populated despite early caller close: %v", err)
	}
}
