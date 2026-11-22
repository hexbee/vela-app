//go:build !darwin && !linux && !windows

package main

import "os/exec"

func isOSSandboxAvailable() bool {
	return false
}

func buildSandboxedShellCommand(command string, _ sandboxSpec) (*exec.Cmd, bool) {
	return buildShellCommand(command), false
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
