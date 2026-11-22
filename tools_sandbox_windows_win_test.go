//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWindowsConfigureProcessGroupRestrictedTokenClearsCreateNoWindow(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/C", "echo", "hi")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Token: syscall.Token(1), // synthetic non-zero token
	}
	configureProcessGroup(cmd)
	if !cmd.SysProcAttr.HideWindow {
		t.Fatalf("expected HideWindow=true")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow != 0 {
		t.Fatalf("expected CREATE_NO_WINDOW to be omitted for restricted token process")
	}
}

func TestWindowsACLSandboxEnforcement(t *testing.T) {
	if !isOSSandboxAvailable() {
		t.Skip("Windows ACL restricted-token sandbox is unavailable in this environment")
	}

	wsDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "blocked.txt")
	insideFile := filepath.Join(wsDir, "allowed.txt")

	// 1. workspace-write allows writing inside workspace and blocks writing outside.
	spec := sandboxSpec{
		mode:           PermissionWorkspaceWrite,
		workspaceRoots: collectSandboxWorkspaceRoots(PermissionWorkspaceWrite, wsDir),
	}
	prepared, err := prepareSandboxedShellCommand("Set-Content -Path '"+insideFile+"' -Value 'ok'", spec, os.Environ())
	if err != nil {
		t.Fatalf("prepareSandboxedShellCommand(inside) failed: %v", err)
	}
	prepared.cmd.Dir = wsDir
	out, runErr := prepared.cmd.CombinedOutput()
	if prepared.cleanup != nil {
		prepared.cleanup()
	}
	if runErr != nil {
		t.Fatalf("expected workspace write to succeed, err=%v output=%s", runErr, string(out))
	}

	preparedOut, err := prepareSandboxedShellCommand("Set-Content -Path '"+outsideFile+"' -Value 'nope'", spec, os.Environ())
	if err != nil {
		t.Fatalf("prepareSandboxedShellCommand(outside) failed: %v", err)
	}
	preparedOut.cmd.Dir = wsDir
	outBlocked, blockedErr := preparedOut.cmd.CombinedOutput()
	if preparedOut.cleanup != nil {
		preparedOut.cleanup()
	}
	if blockedErr == nil {
		t.Fatalf("expected outside write to be blocked, output=%s", string(outBlocked))
	}
	if !looksLikeSandboxDenial(string(outBlocked)) {
		t.Fatalf("expected sandbox denial signature in output, got: %s", string(outBlocked))
	}
}

func TestWindowsSessionTempReuseAndJobObject(t *testing.T) {
	if !isOSSandboxAvailable() {
		t.Skip("Windows ACL restricted-token sandbox is unavailable in this environment")
	}

	wsDir := t.TempDir()
	spec := sandboxSpec{
		sessionID:      "win-reuse-sess",
		mode:           PermissionWorkspaceWrite,
		workspaceRoots: collectSandboxWorkspaceRoots(PermissionWorkspaceWrite, wsDir),
	}
	t.Cleanup(func() { releaseSessionSandboxTemp("win-reuse-sess") })

	p1, err := prepareSandboxedShellCommand("echo $env:TEMP", spec, os.Environ())
	if err != nil {
		t.Fatalf("first prepareSandboxedShellCommand failed: %v", err)
	}
	p1.cmd.Dir = wsDir
	out1, err1 := p1.cmd.CombinedOutput()
	if p1.cleanup != nil {
		p1.cleanup()
	}
	if err1 != nil {
		t.Fatalf("first command failed: %v (%s)", err1, string(out1))
	}

	p2, err := prepareSandboxedShellCommand("echo $env:TEMP", spec, os.Environ())
	if err != nil {
		t.Fatalf("second prepareSandboxedShellCommand failed: %v", err)
	}
	p2.cmd.Dir = wsDir
	out2, err2 := p2.cmd.CombinedOutput()
	if p2.cleanup != nil {
		p2.cleanup()
	}
	if err2 != nil {
		t.Fatalf("second command failed: %v (%s)", err2, string(out2))
	}

	if string(out1) != string(out2) {
		t.Fatalf("expected session-scoped private temp reuse, got %q vs %q", string(out1), string(out2))
	}

	ts := &ToolService{}
	cleaned, cleanErr := ts.CleanupWorkspaceSandboxACL(wsDir)
	if cleanErr != nil || !cleaned {
		t.Fatalf("expected CleanupWorkspaceSandboxACL to remove standing workspace grant, cleaned=%v err=%v", cleaned, cleanErr)
	}
	cleanedAgain, cleanErr2 := ts.CleanupWorkspaceSandboxACL(wsDir)
	if cleanErr2 != nil || cleanedAgain {
		t.Fatalf("expected second CleanupWorkspaceSandboxACL to be a no-op, cleaned=%v err=%v", cleanedAgain, cleanErr2)
	}
}

func TestWindowsProcessTreeKillTerminatesDescendants(t *testing.T) {
	wsDir := t.TempDir()
	pidFile := filepath.Join(wsDir, "child.pid")
	script := "$p = Start-Process -FilePath powershell.exe -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 60' -PassThru -WindowStyle Hidden; Set-Content -Path '" + pidFile + "' -Value $p.Id; Start-Sleep -Seconds 60"
	cmd := buildShellCommand(script)
	cmd.Dir = wsDir

	releaseJob, err := startCommandWithProcessTree(cmd)
	if err != nil {
		t.Fatalf("startCommandWithProcessTree failed: %v", err)
	}
	defer func() {
		if releaseJob != nil {
			releaseJob()
		}
	}()

	var childPID int
	for i := 0; i < 40; i++ {
		if data, rErr := os.ReadFile(pidFile); rErr == nil {
			if n, pErr := strconv.Atoi(strings.TrimSpace(string(data))); pErr == nil && n > 0 {
				childPID = n
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if childPID == 0 {
		killProcessTree(cmd)
		_ = cmd.Wait()
		t.Fatalf("timed out waiting for child process PID file")
	}

	killProcessTree(cmd)
	_ = cmd.Wait()

	// Verify the spawned child process is also gone.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h, openErr := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(childPID))
		if openErr != nil {
			return // process handle no longer exists -> killed
		}
		waitRes, _ := syscall.WaitForSingleObject(h, 0)
		_ = syscall.CloseHandle(h)
		if waitRes == syscall.WAIT_OBJECT_0 {
			return // signaled -> exited
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child process %d survived killProcessTree", childPID)
}
