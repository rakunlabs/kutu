package ops

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

const maxStartDelay = time.Minute

// Scheduler runs keyed periodic jobs. Each job runs sequentially in
// its own goroutine (no overlapping runs of the same job); the first
// run is delayed by up to 10% of the interval (capped at one minute)
// and every following wait adds up to 10% jitter so jobs started
// together spread out. A panicking job is recovered.
type Scheduler struct {
	mu      sync.Mutex
	jobs    map[string]*job
	ctx     context.Context
	running bool
	wg      sync.WaitGroup
}

type job struct {
	interval time.Duration
	fn       func(ctx context.Context)
	cancel   context.CancelFunc
}

// NewScheduler returns an idle scheduler.
func NewScheduler() *Scheduler {
	return &Scheduler{jobs: map[string]*job{}}
}

// Set adds or replaces the job under key. interval <= 0 removes it.
// A replaced job's in-flight run receives a cancelled context.
func (s *Scheduler) Set(key string, interval time.Duration, fn func(ctx context.Context)) {
	if interval <= 0 || fn == nil {
		s.Remove(key)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.jobs[key]; ok && old.cancel != nil {
		old.cancel()
	}
	j := &job{interval: interval, fn: fn}
	s.jobs[key] = j
	if s.running {
		s.start(j)
	}
}

// Remove stops and deletes the job under key.
func (s *Scheduler) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[key]; ok {
		if j.cancel != nil {
			j.cancel()
		}
		delete(s.jobs, key)
	}
}

// Keys returns the registered job keys.
func (s *Scheduler) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.jobs))
	for k := range s.jobs {
		out = append(out, k)
	}
	return out
}

// Run starts every job and blocks until ctx is cancelled and all
// in-flight runs have returned.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		<-ctx.Done()
		return
	}
	s.ctx = ctx
	s.running = true
	for _, j := range s.jobs {
		s.start(j)
	}
	s.mu.Unlock()

	<-ctx.Done()

	s.mu.Lock()
	s.running = false
	for _, j := range s.jobs {
		if j.cancel != nil {
			j.cancel()
			j.cancel = nil
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// start launches j's loop; s.mu must be held.
func (s *Scheduler) start(j *job) {
	ctx, cancel := context.WithCancel(s.ctx)
	j.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		wait := startDelay(j.interval)
		for {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			runSafe(ctx, j.fn)
			wait = j.interval + jitter(j.interval)
		}
	}()
}

func runSafe(ctx context.Context, fn func(context.Context)) {
	defer func() { _ = recover() }()
	fn(ctx)
}

func startDelay(interval time.Duration) time.Duration {
	limit := interval / 10
	if limit > maxStartDelay {
		limit = maxStartDelay
	}
	if limit <= 0 {
		return 0
	}
	return rand.N(limit)
}

func jitter(interval time.Duration) time.Duration {
	limit := interval / 10
	if limit <= 0 {
		return 0
	}
	return rand.N(limit)
}
