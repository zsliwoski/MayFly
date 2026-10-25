package controller

import (
	"fmt"
	"time"
)

// computeExpiry returns created plus the TTL.
func computeExpiry(created time.Time, ttl string) (time.Time, error) {
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid ttl %q: %w", ttl, err)
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("invalid ttl %q: must be positive", ttl)
	}
	return created.Add(d), nil
}

// timeUntil returns the time left before expiresAt and whether it has passed.
func timeUntil(expiresAt, now time.Time) (remaining time.Duration, expired bool) {
	remaining = expiresAt.Sub(now)
	return remaining, remaining <= 0
}
