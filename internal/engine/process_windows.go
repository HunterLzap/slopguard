//go:build windows

package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	createNewProcessGroupFlag  = 0x00000200
	jobObjectExtendedLimitInfo = 9
	jobObjectLimitKillOnClose  = 0x00002000
	processSetQuota            = 0x0100
	processTerminate           = 0x0001
)

// JOBOBJECT_BASIC_LIMIT_INFORMATION（64 位布局；Windows amd64/arm64 一致）。
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

// newProcessCommand 创建 Windows 命令。
// Go 的 exec.Command 对 .bat/.cmd 使用 CommandLineToArgvW 风格引号，而 cmd.exe
// 的解析规则不同：当脚本绝对路径与参数都含空格时会截断路径。这里对批处理
// 显式走 cmd.exe /d /v:off /s /c，并用 cmd 规则手工构造完整命令行。
func newProcessCommand(path string, args []string) *exec.Cmd {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".bat" && ext != ".cmd" {
		return exec.Command(path, args...)
	}
	interpreter, err := commandInterpreter()
	if err != nil {
		// 让 Start 返回明确错误；正常 Windows 上 ComSpec 一定存在。
		return exec.Command(path, args...)
	}
	cmd := exec.Command(interpreter)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: buildBatchCommandLine(interpreter, path, args)}
	return cmd
}

func commandInterpreter() (string, error) {
	if comspec := strings.TrimSpace(os.Getenv("ComSpec")); comspec != "" {
		if resolved, err := exec.LookPath(comspec); err == nil {
			return resolved, nil
		}
	}
	return exec.LookPath("cmd.exe")
}

// buildBatchCommandLine 生成 cmd.exe 能正确解析的命令行：
//
//	"cmd.exe" /d /v:off /s /c ""脚本路径" "参数1" "参数2""
//
// 外层一对引号配合 /s，保证首尾引号被正确剥离。
func buildBatchCommandLine(interpreter, batchPath string, args []string) string {
	var b strings.Builder
	b.WriteString(quoteCmdArg(interpreter))
	b.WriteString(" /d /v:off /s /c ")
	b.WriteByte('"')
	b.WriteString(quoteCmdArg(batchPath))
	for _, arg := range args {
		b.WriteByte(' ')
		b.WriteString(quoteCmdArg(arg))
	}
	b.WriteByte('"')
	return b.String()
}

// quoteCmdArg 用 cmd.exe 规则引用单个参数。参数中的双引号用两个双引号表示。
func quoteCmdArg(value string) string {
	if value == "" {
		return `""`
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroupFlag
}

// processHandle 优先使用 Job Object（KILL_ON_JOB_CLOSE）管理进程树；
// Job 分配失败时回退到 taskkill /T /F /PID。
type processHandle struct {
	pid int
	cmd *exec.Cmd
	job syscall.Handle
}

func attachProcess(cmd *exec.Cmd) *processHandle {
	if cmd == nil || cmd.Process == nil {
		return &processHandle{}
	}
	ph := &processHandle{pid: cmd.Process.Pid, cmd: cmd}
	job := createKillOnCloseJob()
	if job == 0 {
		return ph
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(ph.pid))
	if err != nil {
		syscall.CloseHandle(job)
		return ph
	}
	defer syscall.CloseHandle(h)
	if r1, _, _ := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h)); r1 == 0 {
		syscall.CloseHandle(job)
		return ph
	}
	ph.job = job
	return ph
}

func createKillOnCloseJob() syscall.Handle {
	r1, _, _ := procCreateJobObjectW.Call(0, 0)
	if r1 == 0 {
		return 0
	}
	job := syscall.Handle(r1)
	var info jobObjectExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnClose
	r1, _, _ = procSetInformationJobObject.Call(
		uintptr(job),
		jobObjectExtendedLimitInfo,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r1 == 0 {
		syscall.CloseHandle(job)
		return 0
	}
	return job
}

func (p *processHandle) kill() {
	if p == nil || p.pid <= 0 {
		return
	}
	if p.job != 0 {
		procTerminateJobObject.Call(uintptr(p.job), 1)
	}
	// 双保险：Job 分配失败或子孙在分配前逃逸时，taskkill 仍能按进程树清理。
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	taskkill := filepath.Join(systemRoot, "System32", "taskkill.exe")
	if _, err := os.Stat(taskkill); err == nil {
		cmd := exec.Command(taskkill, "/T", "/F", "/PID", strconv.Itoa(p.pid))
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *processHandle) release() {
	if p != nil && p.job != 0 {
		syscall.CloseHandle(p.job)
		p.job = 0
	}
}
