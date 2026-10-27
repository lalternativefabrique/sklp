package spacemanager

import "time"

// backoff is a per-service capped exponential delay: 1s, 2s, 4s, ... up to a
// ceiling. It stops a crash-looping service from spinning the agent. A
// service that survives long enough is reset to the base by the agent.
type backoff struct {
	base time.Duration
	max  time.Duration
	cur  time.Duration
}

func newBackoff(base, max time.Duration) backoff {
	return backoff{base: base, max: max}
}

// next returns the current delay and advances toward max (doubling).
func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.base
	}
	d := b.cur
	b.cur *= 2
	if b.cur > b.max {
		b.cur = b.max
	}
	return d
}

// reset returns the delay to the base (called after a stable run).
func (b *backoff) reset() { b.cur = 0 }
