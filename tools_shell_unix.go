//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func buildShellCommand(command string) *exec.Cmd {
	shell := "/bin/bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "/bin/sh"
	}
	return exec.Command(shell, "-c", command)
}

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func attachProcessTree(_ *exec.Cmd) func() {
	return nil
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	// Negative PID kills the entire POSIX process group.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
