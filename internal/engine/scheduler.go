// Package engine 提供 SlopGuard 的确定性执行底座：有界并发调度、
// 外部命令执行、Git 变更解析与 baseline 棘轮比对。
package engine

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// DefaultConcurrency 是外部命令并发执行的默认上限。
// 架构要求默认最多同时运行 2 个外部子进程，避免 tsc / eslint 同时拉起导致内存飙升。
const DefaultConcurrency = 2

// ErrNilTask 表示调度任务为 nil。
var ErrNilTask = errors.New("engine: nil task")

// Task 是一个可被调度执行的并发任务。
// 任务必须尊重传入的 ctx；ctx 取消后应尽快返回，由 Runner 负责连带清理子进程。
type Task func(ctx context.Context) error

// Scheduler 是固定上限的并发调度器。
// 它只负责并发与取消传播，不直接创建进程；外部命令统一由 contract.Runner 执行。
type Scheduler struct {
	limit int
}

// NewScheduler 创建调度器。limit <= 0 时使用 DefaultConcurrency。
func NewScheduler(limit int) *Scheduler {
	if limit <= 0 {
		limit = DefaultConcurrency
	}
	return &Scheduler{limit: limit}
}

// Limit 返回实际并发上限。
func (s *Scheduler) Limit() int {
	if s == nil || s.limit <= 0 {
		return DefaultConcurrency
	}
	return s.limit
}

// Run 以有界并发执行 tasks，并按输入下标返回每个任务的错误。
// ctx 取消后不再派发新任务，未开始的任务返回 ctx.Err()；
// 已运行的任务通过 ctx 收到取消信号，由调用方（通常是 Runner）完成子进程组清理。
func (s *Scheduler) Run(ctx context.Context, tasks []Task) []error {
	errs := make([]error, len(tasks))
	if len(tasks) == 0 {
		return errs
	}
	if ctx == nil {
		ctx = context.Background()
	}

	workers := s.Limit()
	if workers > len(tasks) {
		workers = len(tasks)
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	// startMu/nextStart 保证任务按输入下标依次开始执行（不等待前序完成），
	// 这样外部命令的启动顺序可预测，同时仍保留有界并发。
	var startMu sync.Mutex
	startCond := sync.NewCond(&startMu)
	nextStart := 0
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for idx := range jobs {
				startMu.Lock()
				for idx != nextStart {
					startCond.Wait()
				}
				nextStart++
				startCond.Broadcast()
				startMu.Unlock()

				if err := ctx.Err(); err != nil {
					errs[idx] = err
					continue
				}
				task := tasks[idx]
				if task == nil {
					errs[idx] = ErrNilTask
					continue
				}
				errs[idx] = task(ctx)
			}
		}()
	}

dispatch:
	for i := range tasks {
		select {
		case jobs <- i:
		case <-ctx.Done():
			for j := i; j < len(tasks); j++ {
				errs[j] = ctx.Err()
			}
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	return errs
}

// FirstError 返回按任务下标顺序的第一个非 nil 错误。
func FirstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// SignalContext 返回在收到 Ctrl+C（os.Interrupt）或 SIGTERM 时取消的上下文。
// CLI 入口应使用它包裹整次运行的 context，实现信号优雅退出：停止派发新任务，
// 并把取消传播到 Runner，由 Runner 清理子进程及其子孙进程。
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
