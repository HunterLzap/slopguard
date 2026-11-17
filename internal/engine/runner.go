package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

const (
	// MaxOutputBytes 是单个子进程 stdout / stderr 各自保留的最大字节数（2 MiB）。
	// 超出部分被截断，仅保留首部与尾部摘要。
	MaxOutputBytes = 2 * 1024 * 1024

	// DefaultTimeout 是 RunSpec.Timeout 未指定时使用的默认超时。
	DefaultTimeout = 2 * time.Minute

	// readDrainGrace 是进程退出后等待输出管道排空的宽限时间。
	// 超过后关闭读端，避免后台孙进程长期持有管道导致 Run 悬挂。
	readDrainGrace = 2 * time.Second
)

// ErrEmptyCommand 表示 RunSpec.Name 为空。
var ErrEmptyCommand = errors.New("engine: empty command name")

// ExecRunner 是 contract.Runner 的生产实现。
// 它使用环回缓冲流式读取 stdout/stderr（绝不 io.ReadAll），
// 并为每个子进程建立独立进程组，超时或 ctx 取消时连带清理子孙进程。
type ExecRunner struct {
	// DefaultTimeout 在 RunSpec.Timeout <= 0 时生效；<= 0 时使用 DefaultTimeout。
	DefaultTimeout time.Duration
	// MaxOutputBytes 覆盖单流输出上限（测试用）；<= 0 时使用 MaxOutputBytes。
	MaxOutputBytes int
	// LookPathFunc 覆盖可执行文件探测（测试用）；nil 时使用 exec.LookPath。
	LookPathFunc func(string) (string, error)
}

// NewExecRunner 创建生产版 Runner。
func NewExecRunner() *ExecRunner {
	return &ExecRunner{
		DefaultTimeout: DefaultTimeout,
		MaxOutputBytes: MaxOutputBytes,
	}
}

// LookPath 探测工具是否存在，用于优雅降级。
func (r *ExecRunner) LookPath(name string) (string, bool) {
	if strings.TrimSpace(name) == "" {
		return "", false
	}
	look := r.LookPathFunc
	if look == nil {
		look = exec.LookPath
	}
	path, err := look(name)
	if err != nil {
		return "", false
	}
	return path, true
}

// Run 执行一次外部命令。输出以 bufio.Scanner 分块流式读取并限制大小；
// 超时或 ctx 取消时返回 ExitCode = -1，并已清理整个进程组。
func (r *ExecRunner) Run(ctx context.Context, spec contract.RunSpec) contract.RunResult {
	started := time.Now()
	result := contract.RunResult{ExitCode: -1}
	finish := func() contract.RunResult {
		result.Duration = time.Since(started)
		return result
	}

	name := strings.TrimSpace(spec.Name)
	if name == "" {
		result.Err = ErrEmptyCommand
		return finish()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	path, ok := r.LookPath(name)
	if !ok {
		// 工具缺失属于可优雅降级的场景，不是系统错误。
		result.Skipped = true
		return finish()
	}
	if err := ctx.Err(); err != nil {
		result.Err = fmt.Errorf("engine: %s canceled before start: %w", name, err)
		return finish()
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = r.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newProcessCommand(path, spec.Args)
	cmd.Dir = spec.Dir
	cmd.Env = mergeEnv(spec.Env)
	configureProcessGroup(cmd)

	outR, outW, err := os.Pipe()
	if err != nil {
		result.Err = fmt.Errorf("engine: create stdout pipe for %s: %w", name, err)
		return finish()
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		result.Err = fmt.Errorf("engine: create stderr pipe for %s: %w", name, err)
		return finish()
	}
	defer errR.Close()
	defer errW.Close()

	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		outW.Close()
		errW.Close()
		result.Err = fmt.Errorf("engine: start %s: %w", name, err)
		return finish()
	}
	// 父进程不再需要写端；子进程退出（含子孙关闭）后读端自然 EOF。
	outW.Close()
	errW.Close()

	proc := attachProcess(cmd)
	defer proc.release()

	maxOut := r.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = MaxOutputBytes
	}
	outBuf := newCappedBuffer(maxOut)
	errBuf := newCappedBuffer(maxOut)

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		scanStream(outR, outBuf)
	}()
	go func() {
		defer readers.Done()
		scanStream(errR, errBuf)
	}()
	readDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readDone)
	}()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var waitErr error
	canceled := false
	select {
	case waitErr = <-waitCh:
	case <-runCtx.Done():
		canceled = true
		proc.kill()
		waitErr = <-waitCh
	}

	// 进程退出后给输出管道一点排空时间；后台孙进程仍持有管道时强制关闭读端。
	select {
	case <-readDone:
	case <-time.After(readDrainGrace):
		proc.kill()
		_ = outR.Close()
		_ = errR.Close()
		<-readDone
	}

	result.Stdout = outBuf.String()
	result.Stderr = errBuf.String()

	if canceled {
		result.ExitCode = -1
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			result.Err = fmt.Errorf("engine: %s timed out after %s", name, timeout)
		} else {
			result.Err = fmt.Errorf("engine: %s canceled: %w", name, runCtx.Err())
		}
		return finish()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			if result.ExitCode == 0 {
				result.ExitCode = 1
			}
			return finish()
		}
		result.Err = fmt.Errorf("engine: wait %s: %w", name, waitErr)
		return finish()
	}
	result.ExitCode = 0
	return finish()
}

// mergeEnv 把 spec.Env 合并到当前进程环境；nil 表示完全继承。
func mergeEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	env := os.Environ()
	index := make(map[string]int, len(env))
	for i, kv := range env {
		if j := strings.IndexByte(kv, '='); j > 0 {
			index[normalizeEnvKey(kv[:j])] = i
		}
	}
	for k, v := range extra {
		key := normalizeEnvKey(k)
		entry := k + "=" + v
		if i, ok := index[key]; ok {
			env[i] = entry
			continue
		}
		env = append(env, entry)
		index[key] = len(env) - 1
	}
	return env
}

func normalizeEnvKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

// scanStream 使用 bufio.Scanner 分块流式读取，内存占用与单行长度无关。
func scanStream(r io.Reader, buf *cappedBuffer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 32*1024), 64*1024)
	scanner.Split(splitOutputChunks)
	for scanner.Scan() {
		_, _ = buf.Write(scanner.Bytes())
	}
}

// splitOutputChunks 以换行或 32KiB 为界切块，保证超长单行也能流式消费。
func splitOutputChunks(data []byte, atEOF bool) (advance int, token []byte, err error) {
	const chunk = 32 * 1024
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 && i < chunk {
		return i + 1, data[:i+1], nil
	}
	if len(data) >= chunk {
		return chunk, data[:chunk], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// cappedBuffer 保留输出首部与尾部，中间溢出部分丢弃并记录摘要。
type cappedBuffer struct {
	head      []byte
	tail      []byte
	headLimit int
	tailLimit int
	total     int64
}

func newCappedBuffer(max int) *cappedBuffer {
	if max < 0 {
		max = 0
	}
	head := max / 2
	tail := max - head
	// 为截断标记预留空间，保证 String() ≤ max。
	const markerReserve = 96
	if tail > markerReserve {
		tail -= markerReserve
	} else {
		tail = 0
	}
	return &cappedBuffer{headLimit: head, tailLimit: tail}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	if len(b.head) < b.headLimit {
		take := b.headLimit - len(b.head)
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 && b.tailLimit > 0 {
		b.tail = append(b.tail, p...)
		if len(b.tail) > b.tailLimit {
			b.tail = b.tail[len(b.tail)-b.tailLimit:]
		}
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	if b.total <= int64(len(b.head)+len(b.tail)) {
		return string(b.head) + string(b.tail)
	}
	dropped := b.total - int64(len(b.head)) - int64(len(b.tail))
	var sb strings.Builder
	sb.Grow(len(b.head) + len(b.tail) + 64)
	sb.Write(b.head)
	fmt.Fprintf(&sb, "\n... [slopguard: %d bytes truncated] ...\n", dropped)
	sb.Write(b.tail)
	return sb.String()
}
