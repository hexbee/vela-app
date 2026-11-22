//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// createNoWindow is the Win32 CREATE_NO_WINDOW process creation flag.
// Combined with HideWindow, it prevents GUI-launched apps from spawning
// visible console windows for PowerShell, MCP stdio servers, and taskkill.
const createNoWindow uint32 = 0x08000000

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
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	killCmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	configureProcessGroup(killCmd)
	_ = killCmd.Run()
}
