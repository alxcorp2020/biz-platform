package common

import (
	"context"
	"sync"
	"time"
)

// RateLimiter is a simple token-bucket limiter used to respect each
// data source's per-second and per-day call limits (spec 6.3).
type RateLimiter struct {
	mu           sync.Mutex
	tokens       float64
	maxTokens    float64
	refillPerSec float64
	lastRefill   time.Time

	dailyLimit int
	dailyCount int
	dailyReset time.Time
}

// NewRateLimiter creates a limiter allowing `perSecond` calls/sec (burst =
// perSecond) and at most `perDay` calls per rolling 24h window. perDay <= 0
// means "no daily cap".
func NewRateLimiter(perSecond float64, perDay int) *RateLimiter {
	return &RateLimiter{
		tokens:       perSecond,
		maxTokens:    perSecond,
		refillPerSec: perSecond,
		lastRefill:   time.Now(),
		dailyLimit:   perDay,
		dailyReset:   time.Now().Add(24 * time.Hour),
	}
}

// Wait blocks until a call slot is available or ctx is cancelled.
func (r *RateLimiter) Wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		now := time.Now()

		if now.After(r.dailyReset) {
			r.dailyCount = 0
			r.dailyReset = now.Add(24 * time.Hour)
		}
		if r.dailyLimit > 0 && r.dailyCount >= r.dailyLimit {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Until(r.dailyReset)):
				continue
			}
		}

		elapsed := now.Sub(r.lastRefill).Seconds()
		r.tokens = min(r.maxTokens, r.tokens+elapsed*r.refillPerSec)
		r.lastRefill = now

		if r.tokens >= 1 {
			r.tokens--
			r.dailyCount++
			r.mu.Unlock()
			return nil
		}

		wait := time.Duration((1 - r.tokens) / r.refillPerSec * float64(time.Second))
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Slowdown reduces the effective rate temporarily, e.g. when a source starts
// returning elevated latency or error rates (스펙 6.3 "응답 지연 시 자동 감속").
func (r *RateLimiter) Slowdown(factor float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if factor <= 0 {
		factor = 0.5
	}
	r.refillPerSec = r.refillPerSec * factor
	if r.refillPerSec < 0.1 {
		r.refillPerSec = 0.1
	}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
