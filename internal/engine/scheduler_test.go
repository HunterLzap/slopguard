package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewSchedulerLimit(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{0, DefaultConcurrency},
		{-1, DefaultConcurrency},
		{1, 1},
		{4, 4},
	}
	for _, tt := range tests {
		if got := NewScheduler(tt.in).Limit(); got != tt.want {
			t.Errorf("NewScheduler(%d).Limit() = %d, want %d", tt.in, got, tt.want)
		}
	}
	var nilScheduler *Scheduler
	if got := nilScheduler.Limit(); got != DefaultConcurrency {
		t.Errorf("nil Scheduler Limit() = %d, want %d", got, DefaultConcurrency)
	}
}

func TestSchedulerRunBoundsConcurrency(t *testing.T) {
	const limit = 3
	const total = 9

	s := NewScheduler(limit)
	started := make(chan struct{}, total)
	release := make(chan struct{})
	var current, maxSeen int32
	tasks := make([]Task, total)
	for i := range tasks {
		tasks[i] = func(context.Context) error {
			c := atomic.AddInt32(&current, 1)
			for {
				m := atomic.LoadInt32(&maxSeen)
				if c <= m || atomic.CompareAndSwapInt32(&maxSeen, m, c) {
					break
				}
			}
			started <- struct{}{}
			<-release
			atomic.AddInt32(&current, -1)
			return nil
		}
	}

	done := make(chan []error, 1)
	go func() { done <- s.Run(context.Background(), tasks) }()

	for i := 0; i < limit; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d tasks started, want %d", i, limit)
		}
	}
	select {
	case <-started:
		t.Fatalf("more than %d tasks ran concurrently", limit)
	case <-time.After(150 * time.Millisecond):
	}
	if got := atomic.LoadInt32(&maxSeen); got != limit {
		t.Fatalf("max concurrency = %d, want %d", got, limit)
	}

	close(release)
	errs := <-done
	if len(errs) != total {
		t.Fatalf("len(errs) = %d, want %d", len(errs), total)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("task %d error: %v", i, err)
		}
	}
}

func TestSchedulerRunEmpty(t *testing.T) {
	if errs := NewScheduler(2).Run(context.Background(), nil); len(errs) != 0 {
		t.Fatalf("len(errs) = %d, want 0", len(errs))
	}
}

func TestSchedulerNilTask(t *testing.T) {
	ran := int32(0)
	tasks := []Task{
		func(context.Context) error { atomic.AddInt32(&ran, 1); return nil },
		nil,
	}
	errs := NewScheduler(1).Run(context.Background(), tasks)
	if errs[0] != nil {
		t.Fatalf("task 0 error = %v", errs[0])
	}
	if !errors.Is(errs[1], ErrNilTask) {
		t.Fatalf("task 1 error = %v, want ErrNilTask", errs[1])
	}
	if ran != 1 {
		t.Fatalf("ran = %d, want 1", ran)
	}
}

func TestSchedulerCancelBeforeRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ran int32
	tasks := make([]Task, 5)
	for i := range tasks {
		tasks[i] = func(context.Context) error { atomic.AddInt32(&ran, 1); return nil }
	}
	errs := NewScheduler(2).Run(ctx, tasks)
	for i, err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("task %d error = %v, want context.Canceled", i, err)
		}
	}
	if ran != 0 {
		t.Fatalf("ran = %d, want 0", ran)
	}
}

func TestSchedulerCancelStopsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	finished := make(chan []error, 1)

	tasks := []Task{
		func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
		func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
		func(context.Context) error { return nil },
	}
	go func() {
		finished <- NewScheduler(2).Run(ctx, tasks)
	}()

	<-started
	<-started
	cancel()

	select {
	case errs := <-finished:
		if !errors.Is(errs[0], context.Canceled) || !errors.Is(errs[1], context.Canceled) {
			t.Fatalf("running task errors = %v, %v; want context.Canceled", errs[0], errs[1])
		}
		if !errors.Is(errs[2], context.Canceled) {
			t.Fatalf("unstarted task error = %v, want context.Canceled", errs[2])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not return after cancellation")
	}
}

func TestFirstError(t *testing.T) {
	errA := errors.New("a")
	errB := errors.New("b")
	if got := FirstError([]error{nil, errA, errB}); !errors.Is(got, errA) {
		t.Fatalf("FirstError = %v, want %v", got, errA)
	}
	if got := FirstError([]error{nil, nil}); got != nil {
		t.Fatalf("FirstError = %v, want nil", got)
	}
}
