package common

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"
)

// ErrorClass distinguishes transient failures (worth retrying) from
// permanent ones (spec 6.4).
type ErrorClass int

const (
	ClassRetryable ErrorClass = iota
	ClassPermanent
)

// ClassifiableError lets a source collector mark an error explicitly.
// If an error returned by fn does not implement this, it defaults to retryable
// (network/timeout errors are the common case and should not implement it).
type ClassifiableError interface {
	error
	ErrorClass() ErrorClass
}

// PermanentError wraps an error to mark it as non-retryable:
// auth failures, malformed requests, 403/404, deprecated APIs (spec 6.4).
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string           { return e.Err.Error() }
func (e *PermanentError) Unwrap() error            { return e.Err }
func (e *PermanentError) ErrorClass() ErrorClass   { return ClassPermanent }

func classify(err error) ErrorClass {
	var ce ClassifiableError
	if errors.As(err, &ce) {
		return ce.ErrorClass()
	}
	return ClassRetryable
}

// RetryConfig controls exponential backoff with jitter.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func DefaultRetryConfig() RetryConfig {
	return RetryConfig{MaxAttempts: 5, BaseDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second}
}

// Do runs fn with exponential backoff + jitter, retrying only on
// ClassRetryable errors. Returns the last error if all attempts fail, or
// immediately for a permanent error.
func Do(ctx context.Context, cfg RetryConfig, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := backoffDelay(cfg, attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		if classify(err) == ClassPermanent {
			return err // do not retry: auth failure, 4xx, deprecated API, etc.
		}
	}
	return lastErr
}

func backoffDelay(cfg RetryConfig, attempt int) time.Duration {
	d := float64(cfg.BaseDelay) * math.Pow(2, float64(attempt-1))
	if d > float64(cfg.MaxDelay) {
		d = float64(cfg.MaxDelay)
	}
	jitter := 0.8 + rand.Float64()*0.4 // +/-20%
	return time.Duration(d * jitter)
}
