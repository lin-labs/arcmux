package daemon

import (
	"context"
	"errors"
	"sync"
)

const goalSummaryQueueCapacity = 64

var errGoalSummaryQueueFull = errors.New("overall-goal summary queue is full")

type goalSummaryJob struct {
	key  string
	run  func(context.Context) error
	done func(error)
}

type goalSummaryPool struct {
	workers int
	jobs    chan goalSummaryJob

	startOnce sync.Once
	wg        sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc

	mu      sync.Mutex
	pending map[string]struct{}
}

func newGoalSummaryPool(workers, queueCapacity int) *goalSummaryPool {
	return &goalSummaryPool{
		workers: workers,
		jobs:    make(chan goalSummaryJob, queueCapacity),
		pending: make(map[string]struct{}),
	}
}

func (p *goalSummaryPool) start(ctx context.Context) {
	p.startOnce.Do(func() {
		p.ctx, p.cancel = context.WithCancel(ctx)
		p.wg.Add(p.workers)
		for range p.workers {
			go p.worker()
		}
	})
}

func (p *goalSummaryPool) enqueue(job goalSummaryJob) error {
	if job.key == "" || job.run == nil {
		return errors.New("invalid overall-goal summary job")
	}
	if p.ctx == nil {
		return errors.New("overall-goal summary pool is not started")
	}
	select {
	case <-p.ctx.Done():
		return context.Canceled
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.pending[job.key]; exists {
		return ErrSummarizeInFlight
	}
	select {
	case p.jobs <- job:
		p.pending[job.key] = struct{}{}
		return nil
	default:
		return errGoalSummaryQueueFull
	}
}

func (p *goalSummaryPool) worker() {
	defer p.wg.Done()
	for {
		select {
		case job := <-p.jobs:
			p.run(job)
		case <-p.ctx.Done():
			p.drainCanceled()
			return
		}
	}
}

func (p *goalSummaryPool) run(job goalSummaryJob) {
	err := job.run(p.ctx)
	p.complete(job, err)
}

func (p *goalSummaryPool) complete(job goalSummaryJob, err error) {
	p.mu.Lock()
	delete(p.pending, job.key)
	p.mu.Unlock()
	if job.done != nil {
		job.done(err)
	}
}

func (p *goalSummaryPool) drainCanceled() {
	for {
		select {
		case job := <-p.jobs:
			p.complete(job, context.Canceled)
		default:
			return
		}
	}
}

func (p *goalSummaryPool) pendingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

func (p *goalSummaryPool) wait() { p.wg.Wait() }

func (p *goalSummaryPool) stop() {
	if p.cancel != nil {
		p.cancel()
	}
}
