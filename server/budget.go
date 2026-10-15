package server

import (
	"context"
	"sync"
)

// byteBudget accounts for file buffers from allocation through completed WS
// writes. Request metadata and transport buffers are deliberately separate.
type byteBudget struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	peak    int64
	changed chan struct{}
}

func newBudget(limit int64) *byteBudget {
	return &byteBudget{limit: limit, changed: make(chan struct{})}
}

func (b *byteBudget) acquire(ctx context.Context, n int64) error {
	for {
		b.mu.Lock()
		if err := ctx.Err(); err != nil {
			b.mu.Unlock()
			return err
		}
		if b.used+n <= b.limit {
			b.used += n
			b.peak = max(b.peak, b.used)
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	close(b.changed)
	b.changed = make(chan struct{})
}
