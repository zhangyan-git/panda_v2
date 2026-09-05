package sync

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Job is a periodically executed unit of work. Run is called once immediately
// when the scheduler starts and once for each interval thereafter.
type Job struct {
	Interval time.Duration
	Run      func(context.Context) error
}

// SchedulerConfig configures the jobs managed by a Scheduler.
type SchedulerConfig struct {
	Jobs []Job
}

// Scheduler runs configured jobs independently. A job's next run is not
// started until its previous run returns, and an error from one job does not
// affect any other job.
type Scheduler struct {
	jobs []Job

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewScheduler(config SchedulerConfig) *Scheduler {
	return &Scheduler{jobs: append([]Job(nil), config.Jobs...)}
}

// Start starts all jobs and returns immediately. Each job runs once before its
// ticker is serviced. Calling Start while the scheduler is running is a no-op.
func (s *Scheduler) Start(ctx context.Context) error {
	if s == nil {
		return errors.New("scheduler is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, job := range s.jobs {
		if job.Interval <= 0 {
			return errors.New("job interval must be positive")
		}
		if job.Run == nil {
			return errors.New("job run function is required")
		}
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.running = true
	done := s.done
	s.mu.Unlock()

	go s.run(runCtx, done)
	return nil
}

// Stop cancels all jobs and waits for every in-flight run to return. It is safe
// to call repeatedly.
func (s *Scheduler) Stop() {
	_ = s.StopContext(context.Background())
}

// StopContext cancels all jobs and waits until they exit or ctx is canceled.
// If ctx expires while a job ignores cancellation, the scheduler remains
// stopped from starting new work but its in-flight goroutine may still finish.
func (s *Scheduler) StopContext(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	cancel()
	select {
	case <-done:
		s.mu.Lock()
		if s.done == done {
			s.running = false
			s.cancel = nil
			s.done = nil
		}
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	var jobs sync.WaitGroup
	jobs.Add(len(s.jobs))
	for _, job := range s.jobs {
		go func(job Job) {
			defer jobs.Done()
			ticker := time.NewTicker(job.Interval)
			defer ticker.Stop()

			s.runJob(ctx, job)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.runJob(ctx, job)
				}
			}
		}(job)
	}
	jobs.Wait()
}

func (s *Scheduler) runJob(ctx context.Context, job Job) {
	if ctx.Err() != nil {
		return
	}
	defer func() {
		// A faulty integration must not terminate the scheduler or other jobs.
		_ = recover()
	}()
	// Errors are intentionally isolated to the job. A job may report or log
	// its own error; the scheduler only controls its lifecycle.
	_ = job.Run(ctx)
}
