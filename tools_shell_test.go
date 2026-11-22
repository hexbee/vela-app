package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withOSSandboxAvailable overrides the package-level OS sandbox probe for a test
// and restores it on cleanup. Tests in this package must not call t.Parallel()
// while mutating osSandboxAvailableFn.
func withOSSandboxAvailable(t *testing.T, available bool) {
	t.Helper()
	orig := osSandboxAvailableFn
	osSandboxAvailableFn = func() bool { return available }
	t.Cleanup(func() { osSandboxAvailableFn = orig })
}

func withOSSandboxDisabled(t *testing.T) {
	t.Helper()
	withOSSandboxAvailable(t, false)
}

func TestBashStrictReadOnlyAllowlist(t *testing.T) {
	withOSSandboxDisabled(t)

	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	allowed := []string{
		"go version",
		"go env -json",
		"ls -la",
		"git status -s",
		"git log -n 5 --oneline",
		"git diff --stat",
		"git branch --show-current",
		"find . -name '*.go' -type f",
		"grep -rn 'TODO' src",
		"pwd",
		"node --version",
		"echo hello world",
	}
	for _, cmd := range allowed {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "s1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionReadOnly,
			Input:          map[string]any{"command": cmd},
		})
		if res.Decision != DecisionAllow {
			t.Errorf("expected read-only allow for %q, got %#v", cmd, res)
		}
	}

	blocked := []string{
		"git -c core.sshCommand=evil status",
		"git diff --output=/tmp/out.txt",
		"git diff --ext-diff",
		"git branch new-branch",
		"git branch -D main",
		"find . -name '*.go' -exec rm {} \\;",
		"find . -delete",
		"go env -w GOPROXY=evil",
		"go test ./...",
		"ls -la | wc -l",
		"echo hi > out.txt",
		"ls -la && rm -rf /",
		"echo $(whoami)",
		"echo `whoami`",
		"tail -f app.log",
		"rm temp.txt",
		"Out-File test.txt",
		"Set-Content test.txt hi",
		"FOO=bar ls",
		"ls --unknown-flag",
	}
	for _, cmd := range blocked {
		// read-only must hard-deny
		ro := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "s1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionReadOnly,
			Input:          map[string]any{"command": cmd},
		})
		if ro.Decision != DecisionDeny {
			t.Errorf("expected read-only deny for %q, got %#v", cmd, ro)
		}

		// workspace-write must require one-time approval (ask)
		ww := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "s1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionWorkspaceWrite,
			Input:          map[string]any{"command": cmd},
		})
		if ww.Decision != DecisionAsk {
			t.Errorf("expected workspace-write ask for %q, got %#v", cmd, ww)
		}
	}
}

func TestBashBackendFailClosedAndEnvSanitize(t *testing.T) {
	withOSSandboxDisabled(t)

	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	// read-only deny cannot be bypassed even with Approved=true
	_, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "s1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionReadOnly,
		Approved:       true,
		Input:          map[string]any{"command": "echo hacked > pwn.txt"},
	})
	if err == nil {
		t.Fatalf("expected hard deny in read-only mode even when Approved=true")
	}

	// workspace-write ask fails when Approved=false
	_, err = ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "s1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       false,
		Input:          map[string]any{"command": "echo hi > out.txt"},
	})
	if err == nil {
		t.Fatalf("expected error when ask command is executed with Approved=false")
	}

	// Sensitive env vars must be stripped before child process runs
	t.Setenv("DEEPSEEK_API_KEY", "sk-super-secret")
	t.Setenv("DB_PASSWORD", "hunter2")
	t.Setenv("AUTH_TOKEN", "tok-123")
	t.Setenv("VELA_SAFE_VAR", "visible")

	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "s1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input:          map[string]any{"command": "env"},
	})
	if err != nil || res.IsError {
		t.Fatalf("env command failed: err=%v res=%#v", err, res)
	}
	for _, secret := range []string{"DEEPSEEK_API_KEY", "sk-super-secret", "DB_PASSWORD", "AUTH_TOKEN"} {
		if strings.Contains(res.ModelContent, secret) {
			t.Fatalf("sensitive env %q leaked into child process output", secret)
		}
	}
	if !strings.Contains(res.ModelContent, "VELA_SAFE_VAR=visible") {
		t.Fatalf("expected non-sensitive env var to remain visible")
	}
}

func TestBashNonZeroExitCodeAndCancelProcessGroup(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	// Non-zero exit code returns as normal (IsError=false) tool result with [exit code: N]
	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "s1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input:          map[string]any{"command": "echo out-msg; echo err-msg >&2; exit 42"},
	})
	if err != nil {
		t.Fatalf("unexpected rpc error for non-zero exit: %v", err)
	}
	if res.IsError {
		t.Fatalf("non-zero exit code should not set IsError=true: %#v", res)
	}
	if !strings.Contains(res.ModelContent, "out-msg") ||
		!strings.Contains(res.ModelContent, "[stderr]\nerr-msg") ||
		!strings.Contains(res.ModelContent, "[exit code: 42]") {
		t.Fatalf("unexpected formatted output:\n%s", res.ModelContent)
	}
	if code, _ := res.UIMeta["exitCode"].(int); code != 42 {
		t.Fatalf("expected uiMeta.exitCode=42, got %#v", res.UIMeta)
	}

	// CancelSessionTools kills a running bash process group promptly
	done := make(chan ToolExecuteResult, 1)
	go func() {
		r, _ := ts.ExecuteTool(ToolExecuteRequest{
			SessionID:      "cancel-sess",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionWorkspaceWrite,
			Approved:       true,
			Input:          map[string]any{"command": "sleep 30"},
		})
		done <- r
	}()

	time.Sleep(150 * time.Millisecond)
	start := time.Now()
	if err := ts.CancelSessionTools("cancel-sess"); err != nil {
		t.Fatalf("CancelSessionTools failed: %v", err)
	}

	select {
	case r := <-done:
		if time.Since(start) > 3*time.Second {
			t.Fatalf("process took too long to terminate after cancel: %v", time.Since(start))
		}
		if !strings.Contains(r.ModelContent, "[cancelled by user]") {
			t.Fatalf("expected cancelled marker, got: %s", r.ModelContent)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("timed out waiting for cancelled bash process to exit")
	}
}

func TestBashScratchCleanupAndSnapshotDiff(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	ts := &ToolService{}

	// 1. Read-only command in temporary chat (Workdir=="") leaves ZERO scratch dir behind.
	res1, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "temp-clean-1",
		ToolName:       "bash",
		Workdir:        "",
		PermissionMode: PermissionWorkspaceWrite,
		Input:          map[string]any{"command": "echo hello-temp"},
	})
	if err != nil || res1.IsError {
		t.Fatalf("temp echo failed: err=%v res=%#v", err, res1)
	}
	if res1.AssignedScratch != "" {
		t.Fatalf("expected empty AssignedScratch for read-only temp command, got %q", res1.AssignedScratch)
	}
	scratchPath := filepath.Join(fakeHome, ".vela", "scratch", "temp-clean-1")
	if _, statErr := os.Stat(scratchPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected empty scratch dir to be cleaned up, statErr=%v", statErr)
	}

	// 2. Command that creates a file in temp chat keeps scratch dir, returns AssignedScratch & producedFiles.
	res2, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "temp-prod-1",
		ToolName:       "bash",
		Workdir:        "",
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input:          map[string]any{"command": "echo 'v1' > first.txt"},
	})
	if err != nil || res2.IsError {
		t.Fatalf("temp file creation failed: err=%v res=%#v", err, res2)
	}
	if res2.AssignedScratch == "" {
		t.Fatalf("expected AssignedScratch to be returned when file was created")
	}
	produced2, _ := res2.UIMeta["producedFiles"].([]string)
	if len(produced2) != 1 || filepath.Base(produced2[0]) != "first.txt" {
		t.Fatalf("expected producedFiles=[first.txt], got %#v", produced2)
	}

	// 3. Subsequent bash call in the SAME bound scratch dir must NOT re-report first.txt!
	res3, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "temp-prod-1",
		ToolName:       "bash",
		Workdir:        res2.AssignedScratch,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input:          map[string]any{"command": "echo 'v2' > second.txt"},
	})
	if err != nil || res3.IsError {
		t.Fatalf("second bash call failed: err=%v res=%#v", err, res3)
	}
	produced3, _ := res3.UIMeta["producedFiles"].([]string)
	if len(produced3) != 1 || filepath.Base(produced3[0]) != "second.txt" {
		t.Fatalf("expected only newly created second.txt (no duplicate first.txt), got %#v", produced3)
	}
}

func TestBashSnapshotSkipsVenvAndCapsProducedFiles(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	// 1. Creating .venv, __pycache__, and a custom-named venv (with pyvenv.cfg) should not flood producedFiles,
	// while regular project files created in the same command are still reported.
	cmd := strings.Join([]string{
		"mkdir -p .venv/lib/python3.12/site-packages/pkg __pycache__ custom_env/lib",
		"echo 'home = /usr/bin' > .venv/pyvenv.cfg",
		"echo 'x = 1' > .venv/lib/python3.12/site-packages/pkg/mod.py",
		"echo 'bytecode' > __pycache__/app.cpython-312.pyc",
		"echo 'home = /usr/bin' > custom_env/pyvenv.cfg",
		"echo 'y = 2' > custom_env/lib/site.py",
		"echo 'print(1)' > app.py",
	}, " && ")

	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "venv-filter-1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input:          map[string]any{"command": cmd},
	})
	if err != nil || res.IsError {
		t.Fatalf("venv creation command failed: err=%v res=%#v", err, res)
	}
	produced, _ := res.UIMeta["producedFiles"].([]string)
	if len(produced) != 1 || filepath.Base(produced[0]) != "app.py" {
		t.Fatalf("expected only [app.py] in producedFiles, got %#v", produced)
	}

	// 2. diffSnapshots caps output at maxProducedFilesPerCommand when many regular files change.
	before := map[string]fileStamp{}
	after := make(map[string]fileStamp, maxProducedFilesPerCommand+25)
	for i := 0; i < maxProducedFilesPerCommand+25; i++ {
		after[filepath.Join("gen", strings.Repeat("a", 2)+string(rune('0'+i/10))+string(rune('0'+i%10))+".txt")] = fileStamp{
			size:        10,
			modTimeNano: int64(i + 1),
		}
	}
	capped := diffSnapshots(wsDir, before, after)
	if len(capped) != maxProducedFilesPerCommand {
		t.Fatalf("expected diffSnapshots to cap at %d files, got %d", maxProducedFilesPerCommand, len(capped))
	}
}
