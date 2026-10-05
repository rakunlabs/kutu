package service

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// tokenUsageFlushInterval is how often recorded token uses are persisted.
const tokenUsageFlushInterval = time.Minute

// tokenUsage records the last time each API token authenticated. Uses
// are kept in memory and flushed in batches so the hot /data path never
// waits on a write (and a cluster doesn't sync on every request).
type tokenUsage struct {
	mu      sync.Mutex
	pending map[string]time.Time // token ID -> last use, not yet persisted
	seen    map[string]time.Time // token ID -> last use observed by this process
}

func newTokenUsage() *tokenUsage {
	return &tokenUsage{
		pending: make(map[string]time.Time),
		seen:    make(map[string]time.Time),
	}
}

func (u *tokenUsage) record(id string) {
	if u == nil {
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	u.mu.Lock()
	u.pending[id] = now
	u.seen[id] = now
	u.mu.Unlock()
}

func (u *tokenUsage) take() map[string]time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.pending) == 0 {
		return nil
	}
	out := u.pending
	u.pending = make(map[string]time.Time)
	return out
}

func (u *tokenUsage) lastSeen(id string) (time.Time, bool) {
	if u == nil {
		return time.Time{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	t, ok := u.seen[id]
	return t, ok
}

func (u *tokenUsage) forget(id string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	delete(u.pending, id)
	delete(u.seen, id)
	u.mu.Unlock()
}

// tokenLastUsed merges the persisted value with uses this process has
// seen but not flushed yet, so the list view is current.
func (s *Service) tokenLastUsed(id string, stored *time.Time) *time.Time {
	if t, ok := s.tokenUsage.lastSeen(id); ok && (stored == nil || t.After(*stored)) {
		return &t
	}
	return stored
}

// FlushTokenUsage persists recorded token uses. The store only moves
// last_used_at forward and never re-creates a deleted token.
func (s *Service) FlushTokenUsage(ctx context.Context) {
	batch := s.tokenUsage.take()
	for id, at := range batch {
		if err := s.store.Tokens().TouchLastUsed(ctx, id, at); err != nil {
			slog.Warn("token usage: persisting last_used_at failed", "token_id", id, "error", err)
		}
	}
}

// startTokenUsageFlusher runs FlushTokenUsage periodically and once more
// on stop.
func (s *Service) startTokenUsageFlusher() {
	s.bgWorker.goLoop(func(ctx context.Context) {
		t := time.NewTicker(tokenUsageFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.FlushTokenUsage(flushCtx)
				cancel()
				return
			case <-t.C:
				flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				s.FlushTokenUsage(flushCtx)
				cancel()
			}
		}
	})
}
