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

func startCommandWithProcessTree(cmd *exec.Cmd) (func(), error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return nil, nil
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
