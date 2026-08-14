package debuginfod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"go.kacmar.sk/debuginfod/key"
)

// blockUntilCancelled returns a fetch function that blocks until its context is cancelled,
// then returns ctx.Err. Used to simulate a slow upstream that the race should cancel.
func blockUntilCancelled() func(context.Context, key.Key) (io.ReadCloser, error) {
	return func(ctx context.Context, _ key.Key) (io.ReadCloser, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

func TestRace_FastestWins(t *testing.T) {
	winner := newStubSource(stubBytes(testPayload))
	loser := newStubSource(blockUntilCancelled())

	r := newRace([]source{loser, winner}, discardLogger())
	rc, _, err := r.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := readAndClose(t, rc); !bytes.Equal(got, testPayload) {
		t.Errorf("body = %q, want %q", got, testPayload)
	}
}

// TestRace_WinnerMetadataIsReturned pairs the returned metadata with the body that won.
// The winner is placed last so returning any other source's metadata, or the zero value, fails here.
func TestRace_WinnerMetadataIsReturned(t *testing.T) {
	late := newStubSource(func(ctx context.Context, _ key.Key) (io.ReadCloser, error) {
		<-ctx.Done()
		return io.NopCloser(bytes.NewReader(testPayload)), nil
	})
	late.meta = Metadata{File: "late.debug", Size: 99}
	winner := newStubSource(stubBytes(testPayload))
	winner.meta = Metadata{File: "winner.debug", Size: 7}

	r := newRace([]source{late, winner}, discardLogger())
	rc, meta, err := r.Fetch(context.Background(), testKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	readAndClose(t, rc)

	if meta.File != "winner.debug" || meta.Size != 7 {
		t.Errorf("Meta = %+v, want the winner's (File=winner.debug Size=7)", meta)
	}
}

func TestRace_LosersAreCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var loserDone atomic.Bool
		loser := newStubSource(func(ctx context.Context, _ key.Key) (io.ReadCloser, error) {
			<-ctx.Done()
			loserDone.Store(true)
			return nil, ctx.Err()
		})
		winner := newStubSource(stubBytes(testPayload))

		r := newRace([]source{loser, winner}, discardLogger())
		rc, _, err := r.Fetch(context.Background(), testKey)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		readAndClose(t, rc)

		synctest.Wait()
		if !loserDone.Load() {
			t.Fatal("loser source was not cancelled after winner served")
		}
	})
}

func TestRace_WinnerCloseCancelsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var winnerCtx atomic.Pointer[context.Context]
		winner := newStubSource(func(ctx context.Context, _ key.Key) (io.ReadCloser, error) {
			winnerCtx.Store(&ctx)
			return io.NopCloser(bytes.NewReader(testPayload)), nil
		})

		r := newRace([]source{winner}, discardLogger())
		rc, _, err := r.Fetch(context.Background(), testKey)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}

		ctxPtr := winnerCtx.Load()
		if ctxPtr == nil {
			t.Fatal("winner source was not invoked")
		}
		select {
		case <-(*ctxPtr).Done():
			t.Fatal("winner ctx cancelled before caller closed the body")
		default:
		}

		rc.Close()

		synctest.Wait()
		select {
		case <-(*ctxPtr).Done():
		default:
			t.Fatal("winner ctx was not cancelled after caller closed the body")
		}
	})
}

func TestRace_ErrorPriority(t *testing.T) {
	transientA := errors.New("server A unreachable")
	transientB := errors.New("server B unreachable")

	cases := []struct {
		name         string
		errs         []error
		wantIs       []error
		wantNotIs    []error
		wantContains []string
	}{
		{
			name:         "TransportErrorMakesNotFoundNonAuthoritative",
			errs:         []error{transientA, ErrNotFound},
			wantIs:       []error{transientA},
			wantNotIs:    []error{ErrNotFound},
			wantContains: []string{ErrNotFound.Error()},
		},
		{
			name:         "TransportErrorMakesAuthRequiredNonAuthoritative",
			errs:         []error{transientA, ErrAuthRequired},
			wantIs:       []error{transientA},
			wantNotIs:    []error{ErrAuthRequired},
			wantContains: []string{ErrAuthRequired.Error()},
		},
		{
			name:         "TransientAndBothAuthoritativeSentinels",
			errs:         []error{transientA, ErrNotFound, ErrAuthRequired},
			wantIs:       []error{transientA},
			wantNotIs:    []error{ErrNotFound, ErrAuthRequired},
			wantContains: []string{ErrNotFound.Error(), ErrAuthRequired.Error()},
		},
		{
			name:      "NotFoundBeatsAuthRequired",
			errs:      []error{ErrAuthRequired, ErrNotFound},
			wantIs:    []error{ErrNotFound},
			wantNotIs: []error{ErrAuthRequired},
		},
		{
			name:   "AllNotFound",
			errs:   []error{ErrNotFound, ErrNotFound},
			wantIs: []error{ErrNotFound},
		},
		{
			name:      "AllAuthRequired",
			errs:      []error{ErrAuthRequired, ErrAuthRequired},
			wantIs:    []error{ErrAuthRequired},
			wantNotIs: []error{ErrNotFound},
		},
		{
			name:      "AllTransportErrorsJoined",
			errs:      []error{transientA, transientB},
			wantIs:    []error{transientA, transientB},
			wantNotIs: []error{ErrNotFound, ErrAuthRequired},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sources := make([]source, len(tc.errs))
			for i, e := range tc.errs {
				sources[i] = newStubSource(stubError(e))
			}

			r := newRace(sources, discardLogger())
			_, _, err := r.Fetch(context.Background(), testKey)

			if err == nil {
				t.Fatal("expected error, got nil")
			}
			for _, want := range tc.wantIs {
				if !errors.Is(err, want) {
					t.Errorf("err = %v, want errors.Is(_, %v) == true", err, want)
				}
			}
			for _, notWant := range tc.wantNotIs {
				if errors.Is(err, notWant) {
					t.Errorf("err = %v, must not wrap %v", err, notWant)
				}
			}
			msg := err.Error()
			for _, want := range tc.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("err message %q should contain %q for diagnostics", msg, want)
				}
			}
		})
	}
}

func TestRace_LateLoserBodyIsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closed := make(chan struct{}, 1)
		lateBody := &observableBody{closed: closed}

		winner := newStubSource(stubBytes(testPayload))
		late := newStubSource(func(ctx context.Context, _ key.Key) (io.ReadCloser, error) {
			<-ctx.Done()
			return lateBody, nil
		})

		r := newRace([]source{winner, late}, discardLogger())
		rc, _, err := r.Fetch(context.Background(), testKey)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		readAndClose(t, rc)

		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("late loser body was not closed by drainLosers")
		}
	})
}

// observableBody is an empty ReadCloser that signals when Close is called.
type observableBody struct {
	closed chan<- struct{}
}

func (b *observableBody) Read(p []byte) (int, error) { return 0, io.EOF }

func (b *observableBody) Close() error {
	select {
	case b.closed <- struct{}{}:
	default:
	}
	return nil
}
