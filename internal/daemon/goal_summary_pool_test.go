package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoalSummaryPoolHasFixedWorkersBoundedQueueAndGlobalDedupe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := newGoalSummaryPool(goalSummaryGlobalLimit, goalSummaryQueueCapacity)
	pool.start(ctx)

	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, goalSummaryGlobalLimit)
	release := make(chan struct{})
	run := func(context.Context) error {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		active.Add(-1)
		return nil
	}

	for i := 0; i < goalSummaryGlobalLimit; i++ {
		if err := pool.enqueue(goalSummaryJob{key: fmt.Sprintf("active-%d", i), run: run}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < goalSummaryGlobalLimit; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("fixed workers did not start")
		}
	}

	accepted := 0
	for i := 0; i < goalSummaryQueueCapacity+25; i++ {
		err := pool.enqueue(goalSummaryJob{key: fmt.Sprintf("queued-%d", i), run: run})
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, errGoalSummaryQueueFull):
		default:
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if accepted != goalSummaryQueueCapacity {
		t.Fatalf("accepted queued jobs=%d, want fixed capacity %d", accepted, goalSummaryQueueCapacity)
	}
	if err := pool.enqueue(goalSummaryJob{key: "active-0", run: run}); !errors.Is(err, ErrSummarizeInFlight) {
		t.Fatalf("duplicate key error=%v", err)
	}
	if maximum.Load() != goalSummaryGlobalLimit || pool.pendingCount() != goalSummaryGlobalLimit+goalSummaryQueueCapacity {
		t.Fatalf("maximum=%d pending=%d", maximum.Load(), pool.pendingCount())
	}
	close(release)
	cancel()
	pool.wait()
}
