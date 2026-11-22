package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsSandboxSidDerivation(t *testing.T) {
	ws1 := windowsWorkspaceWriteSid(`C:\Users\Dev\Project`)
	ws2 := windowsWorkspaceWriteSid(`c:\users\dev\project`)
	if ws1 != ws2 {
		t.Fatalf("expected case-insensitive deterministic workspace SID, got %q vs %q", ws1, ws2)
	}
	if !strings.HasPrefix(ws1, "S-1-4-") || strings.Count(ws1, "-") != 4 {
		t.Fatalf("unexpected workspace SID format: %q", ws1)
	}

	tmp := windowsTempWriteSid(`C:\Users\Dev\Project`)
	if tmp == ws1 {
		t.Fatalf("expected temp SID to be domain-separated from workspace SID")
	}
	if !strings.HasPrefix(tmp, "S-1-4-") || !strings.HasSuffix(tmp, "-1") || strings.Count(tmp, "-") != 5 {
		t.Fatalf("unexpected temp SID format: %q", tmp)
	}

	k1 := windowsPathLockKey(`C:\Users\Dev\Project`)
	k2 := windowsPathLockKey(`c:\users\dev\project`)
	if k1 != k2 || len(k1) != 16 {
		t.Fatalf("unexpected lock key: %q vs %q", k1, k2)
	}
}

func TestOverrideEnvVars(t *testing.T) {
	base := []string{"PATH=/usr/bin", "Temp=C:\\OldTemp", "USER=alice"}
	got := overrideEnvVars(base, map[string]string{
		"TMP":  `C:\PrivateTemp`,
		"TEMP": `C:\PrivateTemp`,
	})
	var tmpCount, tempCount int
	for _, entry := range got {
		k, v, _ := strings.Cut(entry, "=")
		switch strings.ToLower(k) {
		case "tmp":
			tmpCount++
			if v != `C:\PrivateTemp` {
				t.Fatalf("expected TMP override, got %q", entry)
			}
		case "temp":
			tempCount++
			if v != `C:\PrivateTemp` {
				t.Fatalf("expected TEMP override, got %q", entry)
			}
		}
	}
	if tmpCount != 1 || tempCount != 1 {
		t.Fatalf("expected exactly one TMP and one TEMP entry, got %#v", got)
	}
}

func TestWindowsDirOverlapAndDenialSignatures(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "sub", "temp")
	sibling := t.TempDir()

	if !isWindowsDirOverlap(root, nested) {
		t.Fatalf("expected nested dir to overlap root")
	}
	if !isWindowsDirOverlap(nested, root) {
		t.Fatalf("expected root to overlap nested dir")
	}
	if isWindowsDirOverlap(root, sibling) {
		t.Fatalf("expected sibling temp dirs not to overlap")
	}

	for _, msg := range []string{
		"Access to the path 'C:\\Windows\\test.txt' is denied.",
		"UnauthorizedAccessException: Access is denied.",
		"Operation not permitted",
		"Read-only file system",
	} {
		if !looksLikeSandboxDenial(msg) {
			t.Fatalf("expected sandbox denial match for %q", msg)
		}
	}
}

func TestWindowsPowerShellApprovalBlacklist(t *testing.T) {
	orig := osSandboxAvailableFn
	osSandboxAvailableFn = func() bool { return true }
	t.Cleanup(func() { osSandboxAvailableFn = orig })

	ts := &ToolService{}
	wsDir := t.TempDir()

	mustAskSandboxed := []string{
		"Remove-Item -Recurse -Force build",
		"ri temp.txt",
		"del temp.log",
		"Invoke-WebRequest https://example.com",
		"iwr https://example.com/status",
		"Invoke-RestMethod -Uri https://api.example.com",
		"irm https://api.example.com/health",
		"Invoke-Expression 'Write-Output ok'",
		"iex 'Write-Output ok'",
		"Start-Process notepad.exe",
		"Stop-Process -Id 1234",
		"taskkill.exe /PID 1234",
		`C:\Windows\System32\icacls.exe .`,
		"takeown.exe /f sample.txt",
		"reg.exe query HKCU\\Environment",
		"sc.exe query wuauserv",
		"netsh interface show interface",
		"bitsadmin /list",
		"certutil.exe -hashfile README.md",
		"go test ./... && iwr https://example.com",
	}
	winRules := defaultSandboxApprovalBlacklistForOS("windows")
	for _, cmd := range mustAskSandboxed {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:         "win-bl-1",
			ToolName:          "bash",
			Workdir:           wsDir,
			PermissionMode:    PermissionWorkspaceWrite,
			ApprovalBlacklist: winRules,
			Input:             map[string]any{"command": cmd},
		})
		if res.Decision != DecisionAsk {
			t.Errorf("expected sandboxed workspace-write ask for %q, got %#v", cmd, res)
		}
	}

	mustAllowSandboxed := []string{
		"go test ./...",
		"pnpm run build",
		"Get-ChildItem -Recurse",
		"Get-Content README.md | Select-String sandbox",
	}
	for _, cmd := range mustAllowSandboxed {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:         "win-bl-1",
			ToolName:          "bash",
			Workdir:           wsDir,
			PermissionMode:    PermissionWorkspaceWrite,
			ApprovalBlacklist: winRules,
			Input:             map[string]any{"command": cmd},
		})
		if res.Decision != DecisionAllow {
			t.Errorf("expected sandboxed workspace-write allow for %q, got %#v", cmd, res)
		}
	}

	mustAskFullAccess := []string{
		"Remove-Item -Recurse -Force build",
		"taskkill.exe /PID 1234",
		"iex 'Write-Output ok'",
		"icacls.exe .",
	}
	for _, cmd := range mustAskFullAccess {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "win-bl-1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionFullAccess,
			Input:          map[string]any{"command": cmd},
		})
		if res.Decision != DecisionAsk {
			t.Errorf("expected full-access ask for high-risk Windows cmd %q, got %#v", cmd, res)
		}
	}
}

func TestWindowsSessionTempKey(t *testing.T) {
	if k := windowsSessionTempKey("", []string{`C:\Work\A`}); k != "" {
		t.Fatalf("expected empty key for empty sessionID, got %q", k)
	}
	k1 := windowsSessionTempKey("sess-1", []string{`C:\Work\A`, `C:\Work\B`})
	k2 := windowsSessionTempKey("sess-1", []string{`c:\work\b`, `c:\work\a`})
	if k1 == "" || k1 != k2 {
		t.Fatalf("expected order/case-insensitive identical key for same session+roots, got %q vs %q", k1, k2)
	}
	kOtherSess := windowsSessionTempKey("sess-2", []string{`C:\Work\A`, `C:\Work\B`})
	if kOtherSess == k1 {
		t.Fatalf("expected different sessions to have distinct temp keys")
	}
	kOtherRoot := windowsSessionTempKey("sess-1", []string{`C:\Work\A`})
	if kOtherRoot == k1 {
		t.Fatalf("expected different workspace roots to have distinct temp keys")
	}
}

func TestWindowsPowerShellReadOnlyAllowlist(t *testing.T) {
	orig := osSandboxAvailableFn
	osSandboxAvailableFn = func() bool { return false }
	t.Cleanup(func() { osSandboxAvailableFn = orig })

	ts := &ToolService{}
	wsDir := t.TempDir()

	allowed := []string{
		"Get-Location",
		"gl",
		"Get-ChildItem -Recurse -File",
		"gci -Path src -Depth 2 -Name",
		"dir -Directory",
		"Get-Content -TotalCount 20 README.md",
		"gc -Raw go.mod",
		"type README.md",
		"Select-String -Pattern TODO -Path tools_policy.go -SimpleMatch",
		"sls -CaseSensitive -List sandbox tools_sandbox.go",
		"Test-Path -PathType Leaf go.mod",
		"Resolve-Path -Relative .",
		"Get-Item go.mod",
		"Get-Command git",
		"Get-Date -Format yyyy-MM-dd",
		"where.exe go",
		"where /r . *.go",
		"Write-Output hello",
	}
	for _, cmd := range allowed {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "win-ro-1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionReadOnly,
			Input:          map[string]any{"command": cmd},
		})
		if res.Decision != DecisionAllow {
			t.Errorf("expected read-only allow for PowerShell cmd %q, got %#v", cmd, res)
		}
	}

	denied := []string{
		"Get-Content -Wait app.log",
		"Get-ChildItem -UnknownFlag",
		"Get-ChildItem -Path {Remove-Item x}",
		"where { $_.Name -eq 'a' }",
		"where -FilterScript foo",
		"Get-Content README.md | Select-String sandbox",
		"Set-Content out.txt hi",
		"Remove-Item temp.txt",
		"Get-Location extra-arg",
	}
	for _, cmd := range denied {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "win-ro-1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionReadOnly,
			Input:          map[string]any{"command": cmd},
		})
		if res.Decision != DecisionDeny {
			t.Errorf("expected read-only deny for %q, got %#v", cmd, res)
		}
	}
}

func TestSandboxEscalationLadderAndFailClosed(t *testing.T) {
	orig := osSandboxAvailableFn
	osSandboxAvailableFn = func() bool { return true }
	t.Cleanup(func() { osSandboxAvailableFn = orig })

	ts := &ToolService{}
	wsDir := t.TempDir()

	// 1. Strictly wider escalation with justification -> DecisionAsk, no SessionAllowKey.
	roToWW := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:      "esc-1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionReadOnly,
		Input: map[string]any{
			"command":             "go test ./...",
			"sandbox_permissions": PermissionWorkspaceWrite,
			"justification":       "Need to run Go tests inside workspace",
		},
	})
	if roToWW.Decision != DecisionAsk || roToWW.SessionAllowKey != "" || !strings.Contains(roToWW.Reason, "Need to run Go tests") {
		t.Fatalf("expected one-time ask with justification for read-only -> workspace-write, got %#v", roToWW)
	}

	wwToFA := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:      "esc-1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Input: map[string]any{
			"command":             "npm install -g foo",
			"sandbox_permissions": PermissionFullAccess,
			"justification":       "Need to write global npm cache outside workspace",
		},
	})
	if wwToFA.Decision != DecisionAsk || wwToFA.SessionAllowKey != "" || !strings.Contains(wwToFA.Reason, "global npm cache") {
		t.Fatalf("expected one-time ask with justification for workspace-write -> full-access, got %#v", wwToFA)
	}

	// 2. Invalid escalations must hard-deny (fail-closed).
	invalidCases := []struct {
		name  string
		base  string
		input map[string]any
	}{
		{
			name: "missing justification",
			base: PermissionWorkspaceWrite,
			input: map[string]any{
				"command":             "echo hi",
				"sandbox_permissions": PermissionFullAccess,
			},
		},
		{
			name: "justification without sandbox_permissions",
			base: PermissionWorkspaceWrite,
			input: map[string]any{
				"command":       "echo hi",
				"justification": "some reason",
			},
		},
		{
			name: "narrower target",
			base: PermissionFullAccess,
			input: map[string]any{
				"command":             "echo hi",
				"sandbox_permissions": PermissionWorkspaceWrite,
				"justification":       "downgrade",
			},
		},
		{
			name: "unknown target",
			base: PermissionWorkspaceWrite,
			input: map[string]any{
				"command":             "echo hi",
				"sandbox_permissions": "root-mode",
				"justification":       "invalid",
			},
		},
	}
	for _, tc := range invalidCases {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "esc-1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: tc.base,
			Input:          tc.input,
		})
		if res.Decision != DecisionDeny {
			t.Errorf("%s: expected DecisionDeny, got %#v", tc.name, res)
		}
	}

	// 3. Repeating current effective mode is a no-op (no approval prompt needed under sandbox).
	sameMode := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:      "esc-1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Input: map[string]any{
			"command":             "go test ./...",
			"sandbox_permissions": PermissionWorkspaceWrite,
		},
	})
	if sameMode.Decision != DecisionAllow {
		t.Fatalf("expected repeating current mode to allow normally, got %#v", sameMode)
	}

	// 4. ExecuteTool rejects unapproved escalation fail-closed.
	if _, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "esc-1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       false,
		Input: map[string]any{
			"command":             "echo hi",
			"sandbox_permissions": PermissionFullAccess,
			"justification":       "test unapproved",
		},
	}); err == nil {
		t.Fatalf("expected ExecuteTool to reject unapproved escalation")
	}

	// 5. Escalation hint guidance is non-empty for restricted modes and empty for full-access.
	if !strings.Contains(sandboxEscalationHint(PermissionReadOnly), PermissionWorkspaceWrite) {
		t.Fatalf("unexpected read-only escalation hint: %q", sandboxEscalationHint(PermissionReadOnly))
	}
	if !strings.Contains(sandboxEscalationHint(PermissionWorkspaceWrite), PermissionFullAccess) {
		t.Fatalf("unexpected workspace-write escalation hint: %q", sandboxEscalationHint(PermissionWorkspaceWrite))
	}
	if sandboxEscalationHint(PermissionFullAccess) != "" {
		t.Fatalf("expected empty escalation hint for full-access")
	}
}

func TestWindowsACLFriendlyErrors(t *testing.T) {
	accessMsg := formatWindowsACLSandboxError("SetNamedSecurityInfoW(grant)", `C:\Locked`, 5)
	if !strings.Contains(accessMsg, "WRITE_DAC/WRITE_OWNER") {
		t.Fatalf("expected actionable permission hint for Win32 error 5, got: %s", accessMsg)
	}
	ntfsMsg := formatWindowsACLSandboxError("SetNamedSecurityInfoW(grant)", `E:\USBDrive`, 50)
	if !strings.Contains(ntfsMsg, "NTFS") || !strings.Contains(ntfsMsg, "FAT32/exFAT") {
		t.Fatalf("expected NTFS/FAT32 guidance for Win32 error 50, got: %s", ntfsMsg)
	}
	lockMsg := formatWindowsACLSandboxError("LockFileEx", `C:\Repo`, 33)
	if !strings.Contains(lockMsg, "exclusive lock") {
		t.Fatalf("expected lock hint for Win32 error 33, got: %s", lockMsg)
	}
}

func TestPlatformSpecificDefaultApprovalBlacklist(t *testing.T) {
	contains := func(list []string, item string) bool {
		for _, v := range list {
			if v == item {
				return true
			}
		}
		return false
	}

	win := defaultSandboxApprovalBlacklistForOS("windows")
	mac := defaultSandboxApprovalBlacklistForOS("darwin")
	lin := defaultSandboxApprovalBlacklistForOS("linux")

	for _, common := range []string{"rm", "git push", "curl", "npm publish"} {
		if !contains(win, common) || !contains(mac, common) || !contains(lin, common) {
			t.Errorf("expected common rule %q on all platforms", common)
		}
	}
	if !contains(win, "remove-item") || !contains(win, "iwr") || !contains(win, "icacls") {
		t.Errorf("expected Windows rules in windows blacklist")
	}
	if contains(win, "sudo") || contains(win, "diskutil") || contains(win, "chmod") {
		t.Errorf("did not expect Unix-only rules in windows blacklist")
	}
	if !contains(mac, "sudo") || !contains(mac, "diskutil") {
		t.Errorf("expected Unix + macOS rules in darwin blacklist")
	}
	if contains(mac, "remove-item") || contains(mac, "ri") || contains(mac, "icacls") {
		t.Errorf("did not expect Windows-only rules in darwin blacklist")
	}
	if !contains(lin, "sudo") || !contains(lin, "chmod") {
		t.Errorf("expected Unix rules in linux blacklist")
	}
	if contains(lin, "diskutil") || contains(lin, "remove-item") || contains(lin, "ri") {
		t.Errorf("did not expect macOS/Windows-only rules in linux blacklist")
	}
}
