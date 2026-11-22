//go:build windows

package main

import (
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW process creation flag.
// Combined with HideWindow, it prevents GUI-launched apps from spawning
// visible console windows for PowerShell, MCP stdio servers, and taskkill.
const createNoWindow uint32 = 0x08000000

const (
	winJobObjectExtendedLimitInfoClass = 9
	winJobObjectLimitKillOnJobClose    = 0x00002000
	winProcessSetQuota                   = 0x0100
	winProcessTerminate                  = 0x0001
)

var (
	shellKernel32DLL           = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW       = shellKernel32DLL.NewProc("CreateJobObjectW")
	procSetInfoJobObject       = shellKernel32DLL.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj  = shellKernel32DLL.NewProc("AssignProcessToJobObject")
	procTerminateJobObject     = shellKernel32DLL.NewProc("TerminateJobObject")
	windowsProcessJobsMu       sync.Mutex
	windowsProcessJobs         = make(map[int]syscall.Handle)
)

type winIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type winJobObjectBasicLimitInformation struct {
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

type winJobObjectExtendedLimitInformation struct {
	BasicLimitInformation winJobObjectBasicLimitInformation
	IoInfo                winIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func buildShellCommand(command string) *exec.Cmd {
	shell := "pwsh"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "powershell.exe"
	}
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", command)
	configureProcessGroup(cmd)
	return cmd
}

func configureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	// Under a Windows WRITE_RESTRICTED + Low integrity token, CREATE_NO_WINDOW
	// causes child processes to crash with STATUS_DLL_INIT_FAILED (0xC0000142).
	// Keep CREATE_NO_WINDOW only for unrestricted processes.
	if cmd.SysProcAttr.Token == 0 {
		cmd.SysProcAttr.CreationFlags |= createNoWindow
	}
}

// attachProcessTree places the started child process into a Windows Job Object
// configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so the entire process tree
// (including grandchildren) is atomically terminated on cancel, completion cleanup,
// or unexpected host exit.
func attachProcessTree(cmd *exec.Cmd) func() {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	pid := cmd.Process.Pid
	job, err := createKillOnCloseJob()
	if err != nil || job == 0 {
		return nil
	}

	procHandle, err := syscall.OpenProcess(winProcessSetQuota|winProcessTerminate, false, uint32(pid))
	if err != nil {
		_ = syscall.CloseHandle(job)
		return nil
	}
	r1, _, _ := procAssignProcessToJobObj.Call(uintptr(job), uintptr(procHandle))
	_ = syscall.CloseHandle(procHandle)
	if r1 == 0 {
		_ = syscall.CloseHandle(job)
		return nil
	}

	windowsProcessJobsMu.Lock()
	windowsProcessJobs[pid] = job
	windowsProcessJobsMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			windowsProcessJobsMu.Lock()
			j, ok := windowsProcessJobs[pid]
			if ok && j == job {
				delete(windowsProcessJobs, pid)
			}
			windowsProcessJobsMu.Unlock()
			if ok && j != 0 {
				_ = syscall.CloseHandle(j)
			}
		})
	}
}

func createKillOnCloseJob() (syscall.Handle, error) {
	r1, _, callErr := procCreateJobObjectW.Call(0, 0)
	if r1 == 0 {
		return 0, callErr
	}
	job := syscall.Handle(r1)
	info := winJobObjectExtendedLimitInformation{
		BasicLimitInformation: winJobObjectBasicLimitInformation{
			LimitFlags: winJobObjectLimitKillOnJobClose,
		},
	}
	setR1, _, setErr := procSetInfoJobObject.Call(
		uintptr(job),
		uintptr(winJobObjectExtendedLimitInfoClass),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	runtime.KeepAlive(&info)
	if setR1 == 0 {
		_ = syscall.CloseHandle(job)
		return 0, setErr
	}
	return job, nil
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}

	windowsProcessJobsMu.Lock()
	job, ok := windowsProcessJobs[pid]
	if ok {
		delete(windowsProcessJobs, pid)
	}
	windowsProcessJobsMu.Unlock()

	if ok && job != 0 {
		_, _, _ = procTerminateJobObject.Call(uintptr(job), 1)
		_ = syscall.CloseHandle(job)
		return
	}

	// Fallback if the process could not be assigned to a Job Object.
	killCmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	configureProcessGroup(killCmd)
	_ = killCmd.Run()
}
