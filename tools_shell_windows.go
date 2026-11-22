//go:build windows

package main

import (
	"fmt"
	"log"
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
	winCreateSuspended                 uint32 = 0x00000004
	winJobObjectExtendedLimitInfoClass uint32 = 9
	winJobObjectLimitKillOnJobClose    uint32 = 0x00002000
	winProcessSetQuota                 uint32 = 0x0100
	winProcessTerminate                uint32 = 0x0001
	winThreadSuspendResume             uint32 = 0x0002
	winTH32CSSnapThread                uint32 = 0x00000004
)

var (
	shellKernel32DLL          = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW      = shellKernel32DLL.NewProc("CreateJobObjectW")
	procSetInfoJobObject      = shellKernel32DLL.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj = shellKernel32DLL.NewProc("AssignProcessToJobObject")
	procTerminateJobObject    = shellKernel32DLL.NewProc("TerminateJobObject")
	procThread32First         = shellKernel32DLL.NewProc("Thread32First")
	procThread32Next          = shellKernel32DLL.NewProc("Thread32Next")
	procOpenThread            = shellKernel32DLL.NewProc("OpenThread")
	procResumeThread          = shellKernel32DLL.NewProc("ResumeThread")

	windowsProcessJobsMu sync.Mutex
	windowsProcessJobs   = make(map[int]syscall.Handle)
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

type winThreadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
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

// startCommandWithProcessTree starts cmd in a suspended state, assigns it to a
// Windows Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE before any user
// code runs (eliminating the race where child processes like python.exe spawn
// before Job assignment), and then resumes the child process.
func startCommandWithProcessTree(cmd *exec.Cmd) (func(), error) {
	if cmd == nil {
		return nil, fmt.Errorf("nil command")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= winCreateSuspended

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	releaseJob := attachProcessTree(cmd)
	if err := resumeProcessThreads(uint32(cmd.Process.Pid)); err != nil {
		killProcessTree(cmd)
		if releaseJob != nil {
			releaseJob()
		}
		return nil, fmt.Errorf("resume process %d: %w", cmd.Process.Pid, err)
	}
	return releaseJob, nil
}

func resumeProcessThreads(pid uint32) error {
	snap, err := syscall.CreateToolhelp32Snapshot(winTH32CSSnapThread, 0)
	if err != nil {
		return fmt.Errorf("CreateToolhelp32Snapshot(thread): %w", err)
	}
	defer syscall.CloseHandle(snap)

	var te winThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	r1, _, callErr := procThread32First.Call(uintptr(snap), uintptr(unsafe.Pointer(&te)))
	if r1 == 0 {
		return fmt.Errorf("Thread32First: %w", callErr)
	}

	resumed := 0
	for {
		if te.OwnerProcessID == pid {
			hThread, _, openErr := procOpenThread.Call(
				uintptr(winThreadSuspendResume),
				0,
				uintptr(te.ThreadID),
			)
			if hThread == 0 {
				return fmt.Errorf("OpenThread(%d): %w", te.ThreadID, openErr)
			}
			prevCount, _, resumeErr := procResumeThread.Call(hThread)
			_ = syscall.CloseHandle(syscall.Handle(hThread))
			if prevCount == ^uintptr(0) {
				return fmt.Errorf("ResumeThread(%d): %w", te.ThreadID, resumeErr)
			}
			resumed++
		}
		te.Size = uint32(unsafe.Sizeof(te))
		nextR1, _, _ := procThread32Next.Call(uintptr(snap), uintptr(unsafe.Pointer(&te)))
		if nextR1 == 0 {
			break
		}
	}
	if resumed == 0 {
		return fmt.Errorf("no threads found for pid %d", pid)
	}
	return nil
}

// attachProcessTree places the child process into a Windows Job Object
// configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so the entire process tree
// (including grandchildren such as python.exe) is atomically terminated on
// cancel, job_kill, completion cleanup, or unexpected host exit.
func attachProcessTree(cmd *exec.Cmd) func() {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	pid := cmd.Process.Pid
	job, err := createKillOnCloseJob()
	if err != nil || job == 0 {
		log.Printf("[windows-job] CreateJobObjectW/SetInformationJobObject failed for pid %d: %v", pid, err)
		return nil
	}

	procHandle, err := syscall.OpenProcess(winProcessSetQuota|winProcessTerminate, false, uint32(pid))
	if err != nil {
		log.Printf("[windows-job] OpenProcess failed for pid %d: %v", pid, err)
		_ = syscall.CloseHandle(job)
		return nil
	}
	r1, _, assignErr := procAssignProcessToJobObj.Call(uintptr(job), uintptr(procHandle))
	_ = syscall.CloseHandle(procHandle)
	if r1 == 0 {
		log.Printf("[windows-job] AssignProcessToJobObject failed for pid %d: %v", pid, assignErr)
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

// collectDescendantPIDs snapshots all descendant PIDs of rootPID (children,
// grandchildren, etc.) via CreateToolhelp32Snapshot before the root is killed.
func collectDescendantPIDs(rootPID uint32) []uint32 {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(snap)

	childrenByParent := make(map[uint32][]uint32)
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := syscall.Process32First(snap, &pe); err != nil {
		return nil
	}
	for {
		if pe.ProcessID != 0 && pe.ParentProcessID != pe.ProcessID {
			childrenByParent[pe.ParentProcessID] = append(childrenByParent[pe.ParentProcessID], pe.ProcessID)
		}
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := syscall.Process32Next(snap, &pe); err != nil {
			break
		}
	}

	var descendants []uint32
	visited := map[uint32]bool{rootPID: true}
	queue := []uint32{rootPID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range childrenByParent[cur] {
			if visited[child] {
				continue
			}
			visited[child] = true
			descendants = append(descendants, child)
			queue = append(queue, child)
		}
	}
	return descendants
}

func terminatePIDBestEffort(pid uint32) {
	if pid == 0 {
		return
	}
	h, err := syscall.OpenProcess(winProcessTerminate, false, pid)
	if err != nil {
		return
	}
	_ = syscall.TerminateProcess(h, 1)
	_ = syscall.CloseHandle(h)
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}

	// Snapshot descendants BEFORE terminating root/job so grandchildren cannot
	// escape even if Job assignment failed or a child broke away.
	descendants := collectDescendantPIDs(uint32(pid))

	windowsProcessJobsMu.Lock()
	job, ok := windowsProcessJobs[pid]
	if ok {
		delete(windowsProcessJobs, pid)
	}
	windowsProcessJobsMu.Unlock()

	if ok && job != 0 {
		_, _, _ = procTerminateJobObject.Call(uintptr(job), 1)
		_ = syscall.CloseHandle(job)
	} else {
		killCmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
		configureProcessGroup(killCmd)
		_ = killCmd.Run()
		terminatePIDBestEffort(uint32(pid))
	}

	// Belt-and-suspenders: terminate any snapshotted descendants (leaf-first).
	for i := len(descendants) - 1; i >= 0; i-- {
		terminatePIDBestEffort(descendants[i])
	}
}
