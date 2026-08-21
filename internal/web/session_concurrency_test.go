package web

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionConcurrencySerializesSameSession(t *testing.T) {
	limiter := newSessionConcurrency()
	releaseFirst, err := limiter.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan func(), 1)
	errs := make(chan error, 1)
	go func() {
		release, err := limiter.Acquire(context.Background(), "session-a")
		if err != nil {
			errs <- err
			return
		}
		acquired <- release
	}()

	select {
	case release := <-acquired:
		release()
		t.Fatal("second request acquired the same session before the first released it")
	case err := <-errs:
		t.Fatalf("second acquire failed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	releaseFirst()

	select {
	case release := <-acquired:
		release()
	case err := <-errs:
		t.Fatalf("second acquire failed after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second request did not acquire the session after release")
	}
}

func TestSessionConcurrencyAllowsDifferentSessions(t *testing.T) {
	limiter := newSessionConcurrency()
	releaseA, err := limiter.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseB, err := limiter.Acquire(ctx, "session-b")
	if err != nil {
		t.Fatalf("different session was blocked: %v", err)
	}
	releaseB()
}

func TestSessionConcurrencyWaitHonorsCancellation(t *testing.T) {
	limiter := newSessionConcurrency()
	release, err := limiter.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(ctx, "session-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want deadline exceeded", err)
	}
}

func TestSessionConcurrencyReleaseIsIdempotent(t *testing.T) {
	limiter := newSessionConcurrency()
	release, err := limiter.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseAgain, err := limiter.Acquire(ctx, "session-a")
	if err != nil {
		t.Fatalf("session was not reusable after release: %v", err)
	}
	releaseAgain()
}
