//go:build linux

package main

import (
	"os/exec"
	"sync"
)

var (
	bwrapProbeOnce sync.Once
	bwrapBinPath   string
	bwrapSupported bool
)

func isBwrapAvailable() (string, bool) {
	bwrapProbeOnce.Do(func() {
		path, err := exec.LookPath("bwrap")
		if err != nil {
			return
		}
		cmd := exec.Command(path, "--ro-bind", "/", "/", "--", "/bin/true")
		if err := cmd.Run(); err == nil {
			bwrapBinPath = path
			bwrapSupported = true
		}
	})
	return bwrapBinPath, bwrapSupported
}

func isOSSandboxAvailable() bool {
	_, ok := isBwrapAvailable()
	return ok
}

func buildSandboxedShellCommand(command string, spec sandboxSpec) (*exec.Cmd, bool) {
	if spec.mode == PermissionFullAccess {
		return buildShellCommand(command), false
	}
	bwrapPath, ok := isBwrapAvailable()
	if !ok {
		return buildShellCommand(command), false
	}
	shell := "/bin/bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "/bin/sh"
	}
	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--die-with-parent",
	}
	if spec.mode == PermissionWorkspaceWrite {
		args = append(args, "--tmpfs", "/tmp")
		for _, root := range spec.writableRoots {
			if root == "/tmp" || root == "/private/tmp" {
				continue
			}
			args = append(args, "--bind", root, root)
		}
	}
	args = append(args, "--", shell, "-c", command)
	return exec.Command(bwrapPath, args...), true
}

func prepareSandboxedShellCommand(command string, spec sandboxSpec, baseEnv []string) (sandboxedShellCommand, error) {
	cmd, sandboxed := buildSandboxedShellCommand(command, spec)
	cmd.Env = baseEnv
	return sandboxedShellCommand{
		cmd:       cmd,
		sandboxed: sandboxed,
	}, nil
}

func releaseSessionSandboxTemp(_ string) {}
func releaseAllSandboxTemps()            {}

func cleanupWorkspaceSandboxACL(_ string) (bool, error) {
	return false, nil
}
