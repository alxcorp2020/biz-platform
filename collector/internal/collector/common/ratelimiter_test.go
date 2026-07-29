package common

import (
	"context"
	"testing"
	"time"
)

func TestRateLimiter_ThrottlesPerSecond(t *testing.T) {
	rl := NewRateLimiter(5, 0) // 5 calls/sec, no daily cap
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 10; i++ {
		if err := rl.Wait(ctx); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	elapsed := time.Since(start)

	// 10 calls at 5/sec burst=5 should take at least ~1 second for the
	// second batch of 5 to refill.
	if elapsed < 900*time.Millisecond {
		t.Fatalf("expected rate limiting to slow down calls, elapsed=%v", elapsed)
	}
}

func TestRateLimiter_RespectsDailyLimit(t *testing.T) {
	rl := NewRateLimiter(1000, 3) // effectively unlimited per-second, 3/day
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	for i := 0; i < 3; i++ {
		if err := rl.Wait(context.Background()); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	// 4th call should block until daily reset (24h away) and hit our short
	// context timeout instead.
	err := rl.Wait(ctx)
	if err == nil {
		t.Fatal("expected 4th call to be blocked by daily limit")
	}
}
