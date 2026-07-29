package common

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDo_RetriesTransientErrorThenSucceeds(t *testing.T) {
	attempts := 0
	err := Do(context.Background(), RetryConfig{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary network error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestDo_DoesNotRetryPermanentError(t *testing.T) {
	attempts := 0
	err := Do(context.Background(), RetryConfig{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}, func() error {
		attempts++
		return &PermanentError{Err: errors.New("401 unauthorized")}
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attempts != 1 {
		t.Fatalf("permanent error must not be retried, got %d attempts", attempts)
	}
}

func TestDo_GivesUpAfterMaxAttempts(t *testing.T) {
	attempts := 0
	err := Do(context.Background(), RetryConfig{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}, func() error {
		attempts++
		return errors.New("still failing")
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if attempts != 4 {
		t.Fatalf("expected exactly MaxAttempts=4 tries, got %d", attempts)
	}
}

func TestDo_ContextCancelledStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := Do(ctx, RetryConfig{MaxAttempts: 100, BaseDelay: 10 * time.Millisecond, MaxDelay: 20 * time.Millisecond}, func() error {
		attempts++
		return errors.New("keep failing")
	})
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if attempts >= 100 {
		t.Fatalf("expected early stop due to cancellation, got %d attempts", attempts)
	}
}
