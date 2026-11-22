//go:build darwin

package main

import (
	"os/exec"
	"sync"
)

const seatbeltBin = "/usr/bin/sandbox-exec"

var (
	seatbeltProbeOnce sync.Once
	seatbeltSupported bool
)

// isSeatbeltAvailable probes whether /usr/bin/sandbox-exec can apply a profile in the current
// process environment. Nested Seatbelt sandboxes (e.g. App Sandbox or sandboxed test runners)
// reject sandbox_apply with exit code 71 (EPERM), in which case we fall back cleanly.
func isSeatbeltAvailable() bool {
	seatbeltProbeOnce.Do(func() {
		if _, err := exec.LookPath(seatbeltBin); err != nil {
			return
		}
		cmd := exec.Command(seatbeltBin, "-p", "(version 1) (allow default)", "/usr/bin/true")
		if err := cmd.Run(); err == nil {
			seatbeltSupported = true
		}
	})
	return seatbeltSupported
}

func isOSSandboxAvailable() bool {
	return isSeatbeltAvailable()
}

func buildSandboxedShellCommand(command string, spec sandboxSpec) (*exec.Cmd, bool) {
	if spec.mode == PermissionFullAccess {
		return buildShellCommand(command), false
	}
	if !isSeatbeltAvailable() {
		return buildShellCommand(command), false
	}
	shell := "/bin/bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "/bin/sh"
	}
	profile := buildSeatbeltProfile(spec)
	return exec.Command(seatbeltBin, "-p", profile, shell, "-c", command), true
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
