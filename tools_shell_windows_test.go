//go:build windows

package main

import (
	"os/exec"
	"testing"
)

func TestWindowsProcessHidesConsoleWindow(t *testing.T) {
	assertHidden := func(t *testing.T, cmd *exec.Cmd) {
		t.Helper()
		if cmd.SysProcAttr == nil {
			t.Fatalf("expected non-nil SysProcAttr")
		}
		if !cmd.SysProcAttr.HideWindow {
			t.Fatalf("expected SysProcAttr.HideWindow=true")
		}
		if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
			t.Fatalf("expected CREATE_NO_WINDOW (0x%x) in CreationFlags, got 0x%x", createNoWindow, cmd.SysProcAttr.CreationFlags)
		}
	}

	t.Run("buildShellCommand", func(t *testing.T) {
		cmd := buildShellCommand("echo hello")
		assertHidden(t, cmd)
	})

	t.Run("configureProcessGroup", func(t *testing.T) {
		cmd := exec.Command("cmd.exe", "/C", "echo", "hello")
		configureProcessGroup(cmd)
		assertHidden(t, cmd)
	})

	t.Run("startMCPStdioClient", func(t *testing.T) {
		client, err := startMCPStdioClient(MCPServerConfig{
			Name:      "test-stdio",
			Transport: MCPTransportStdio,
			Command:   "node",
			Args:      []string{"server.js"},
		}, "node.exe", "", nil)
		if err != nil {
			t.Fatalf("startMCPStdioClient failed: %v", err)
		}
		assertHidden(t, client.cmd)
	})
}
