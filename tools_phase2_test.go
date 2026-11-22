package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBashTimeoutPromotesToBackgroundJob(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	// Run a command that prints immediately, sleeps past timeoutMs, then prints again and exits.
	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "bg-sess-1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input: map[string]any{
			"description": "background smoke test",
			"command":     "echo line-1; sleep 0.4; echo line-2; echo created > bg_out.txt",
			"timeoutMs":   150,
		},
	})
	if err != nil {
		t.Fatalf("unexpected rpc error: %v", err)
	}
	if res.IsError {
		t.Fatalf("timeout-to-background should not be an error: %#v", res)
	}
	jobID, _ := res.UIMeta["backgroundJobId"].(string)
	if jobID == "" {
		t.Fatalf("expected backgroundJobId in uiMeta, got %#v", res.UIMeta)
	}
	if !strings.Contains(res.ModelContent, "moved to background job") {
		t.Fatalf("expected background promotion notice, got: %s", res.ModelContent)
	}

	// ListBackgroundJobs should show the job for this session.
	jobs := ts.ListBackgroundJobs("bg-sess-1")
	if len(jobs) != 1 || jobs[0].ID != jobID {
		t.Fatalf("expected 1 background job %q, got %#v", jobID, jobs)
	}

	// Poll job_output with wait_ms until completed.
	var combined strings.Builder
	combined.WriteString(res.ModelContent)
	var finalStatus string
	var produced []string
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		outRes, oErr := ts.ExecuteTool(ToolExecuteRequest{
			SessionID:      "bg-sess-1",
			ToolName:       "job_output",
			Workdir:        wsDir,
			PermissionMode: PermissionReadOnly, // job_output is allowed even in read-only mode
			Input: map[string]any{
				"job_id":  jobID,
				"wait_ms": 500,
			},
		})
		if oErr != nil {
			t.Fatalf("job_output failed: %v", oErr)
		}
		combined.WriteString("\n")
		combined.WriteString(outRes.ModelContent)
		if pf, ok := outRes.UIMeta["producedFiles"].([]string); ok && len(pf) > 0 {
			produced = append(produced, pf...)
		}
		finalStatus, _ = outRes.UIMeta["jobStatus"].(string)
		if finalStatus == jobStatusCompleted {
			break
		}
	}

	if finalStatus != jobStatusCompleted {
		t.Fatalf("expected job to complete, last status=%q, output=%s", finalStatus, combined.String())
	}
	if !strings.Contains(combined.String(), "line-2") {
		t.Fatalf("expected line-2 in combined background output, got:\n%s", combined.String())
	}
	if len(produced) != 1 || filepath.Base(produced[0]) != "bg_out.txt" {
		t.Fatalf("expected bg_out.txt in producedFiles, got %#v", produced)
	}
}

func TestBashBackgroundJobKill(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "bg-kill-sess",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input: map[string]any{
			"command":   "sleep 30",
			"timeoutMs": 120,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	jobID, _ := res.UIMeta["backgroundJobId"].(string)
	if jobID == "" {
		t.Fatalf("expected backgroundJobId, got %#v", res.UIMeta)
	}

	start := time.Now()
	killRes, kErr := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "bg-kill-sess",
		ToolName:       "job_kill",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Input: map[string]any{
			"job_id": jobID,
		},
	})
	if kErr != nil || killRes.IsError {
		t.Fatalf("job_kill failed: err=%v res=%#v", kErr, killRes)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("job_kill took too long: %v", time.Since(start))
	}
	if st, _ := killRes.UIMeta["jobStatus"].(string); st != jobStatusKilled {
		t.Fatalf("expected jobStatus=killed, got %#v", killRes.UIMeta)
	}
}

func TestSandboxProfileAndMacOSSeatbelt(t *testing.T) {
	roProfile := buildSeatbeltProfile(sandboxSpec{
		mode:            PermissionReadOnly,
		deniedReadPaths: []string{"/Users/demo/.ssh"},
	})
	if !strings.Contains(roProfile, "(deny file-write*)") || strings.Contains(roProfile, "(allow file-write* (subpath") {
		t.Fatalf("unexpected read-only seatbelt profile: %s", roProfile)
	}
	if !strings.Contains(roProfile, `(deny file-read* (subpath "/Users/demo/.ssh"))`) {
		t.Fatalf("expected credential deny file-read* in seatbelt profile: %s", roProfile)
	}

	wwProfile := buildSeatbeltProfile(sandboxSpec{
		mode:            PermissionWorkspaceWrite,
		writableRoots:   []string{"/tmp/ws with \"quotes\""},
		deniedReadPaths: []string{"/Users/demo/.aws"},
	})
	if !strings.Contains(wwProfile, `(subpath "/tmp/ws with \"quotes\"")`) {
		t.Fatalf("expected properly escaped subpath in workspace-write profile: %s", wwProfile)
	}
	if !strings.Contains(wwProfile, `(deny file-read* (subpath "/Users/demo/.aws"))`) {
		t.Fatalf("expected .aws deny file-read* in workspace-write profile: %s", wwProfile)
	}

	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt runtime enforcement test runs on macOS")
	}

	ts := &ToolService{}
	wsDir, outsideDir := setupTestWorkspace(t)

	// 1. workspace-write allows writing inside workspace (sandboxed when Seatbelt is available).
	insideRes, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "sb-1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input:          map[string]any{"command": "echo ok > inside.txt"},
	})
	if err != nil {
		t.Fatalf("inside write rpc error: %v", err)
	}
	if code, _ := insideRes.UIMeta["exitCode"].(int); code != 0 {
		t.Fatalf("expected inside write to succeed, got: %s", insideRes.ModelContent)
	}

	// 2. full-access runs unsandboxed.
	faRes, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "sb-2",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionFullAccess,
		Input:          map[string]any{"command": "echo full"},
	})
	if err != nil {
		t.Fatalf("full-access echo failed: %v", err)
	}
	if sb, _ := faRes.UIMeta["sandboxed"].(bool); sb {
		t.Fatalf("expected sandboxed=false in full-access mode, got %#v", faRes.UIMeta)
	}

	// 3. When Seatbelt is permitted in the current host environment (non-nested),
	// verify live kernel enforcement blocks writes outside writableRoots.
	if !isOSSandboxAvailable() {
		t.Log("seatbelt unavailable in nested sandbox environment; verified profile generation and graceful fallback")
		return
	}
	if sb, _ := insideRes.UIMeta["sandboxed"].(bool); !sb {
		t.Fatalf("expected sandboxed=true when seatbelt is available, got %#v", insideRes.UIMeta)
	}
	outsideTarget := filepath.Join(outsideDir, "blocked.txt")
	cmd, ok := buildSandboxedShellCommand("touch "+outsideTarget, sandboxSpec{
		mode:          PermissionWorkspaceWrite,
		writableRoots: []string{wsDir},
	})
	if !ok {
		t.Fatalf("expected sandbox-exec to be active on macOS")
	}
	outBytes, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("expected sandbox-exec to block write outside writableRoots, but it succeeded")
	}
	if !looksLikeSandboxDenial(string(outBytes)) {
		t.Fatalf("expected Operation not permitted in sandbox stderr, got: %s", string(outBytes))
	}
}

func TestSkillDiscoveryAndProgressiveLoading(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	wsDir := t.TempDir()

	// Create user-level skills: one directory skill and one single-file skill.
	userSkillDir := filepath.Join(fakeHome, ".vela", "skills", "shared-skill")
	if err := os.MkdirAll(userSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userSkillDir, "SKILL.md"), []byte(`---
name: shared-skill
description: User version of shared skill
---
User body instructions.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".vela", "skills", "quick-note.md"), []byte(`---
name: quick-note
description: >
  Multi-line folded
  description text
---
Quick note body.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create project-level skill overriding shared-skill.
	projSkillDir := filepath.Join(wsDir, ".agents", "skills", "shared-skill")
	if err := os.MkdirAll(projSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projSkillDir, "SKILL.md"), []byte(`---
name: shared-skill
description: "Project override description"
---
# Project Instructions
Follow project rules.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := SkillService{}
	catalog, err := svc.ListSkills(wsDir, nil)
	if err != nil {
		t.Fatalf("ListSkills failed: %v", err)
	}
	if len(catalog.Skills) != 2 {
		t.Fatalf("expected 2 deduplicated skills, got %#v", catalog.Skills)
	}
	if catalog.Digest == "" {
		t.Fatalf("expected non-empty digest")
	}

	// Project skill should come first and override user skill.
	if catalog.Skills[0].Name != "shared-skill" || catalog.Skills[0].Scope != skillScopeProject {
		t.Fatalf("expected project shared-skill first, got %#v", catalog.Skills[0])
	}
	if catalog.Skills[0].Description != "Project override description" {
		t.Fatalf("unexpected description: %q", catalog.Skills[0].Description)
	}
	if catalog.Skills[1].Name != "quick-note" || catalog.Skills[1].Description != "Multi-line folded description text" {
		t.Fatalf("unexpected folded description: %#v", catalog.Skills[1])
	}

	// Execute skill tool via ToolService (allowed in read-only mode too).
	ts := &ToolService{}
	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "sk-1",
		ToolName:       "skill",
		Workdir:        wsDir,
		PermissionMode: PermissionReadOnly,
		Input:          map[string]any{"name": "shared-skill"},
	})
	if err != nil || res.IsError {
		t.Fatalf("skill tool failed: err=%v res=%#v", err, res)
	}
	if !strings.Contains(res.ModelContent, "Follow project rules.") {
		t.Fatalf("expected project skill body, got:\n%s", res.ModelContent)
	}
	if scope, _ := res.UIMeta["skillScope"].(string); scope != skillScopeProject {
		t.Fatalf("expected skillScope=project, got %#v", res.UIMeta)
	}

	// Verify custom user directory (e.g. ~/.agent/skills/), symlinks, priority ordering, inspection, and disable toggle.
	realTarget := filepath.Join(t.TempDir(), "linked-skill-src")
	if err := os.MkdirAll(realTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realTarget, "SKILL.md"), []byte(`---
name: linked-agent-skill
description: Skill from ~/.agent/skills via symlink
---
Linked skill body.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentSkillsDir := filepath.Join(fakeHome, ".agent", "skills")
	if err := os.MkdirAll(agentSkillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTarget, filepath.Join(agentSkillsDir, "linked-agent-skill")); err != nil {
		t.Fatal(err)
	}

	customCfg := &SkillConfig{
		ProjectDirs:    []string{".agents/skills", ".vela/skills"},
		UserDirs:       []string{"~/.agent/skills", "~/.vela/skills"},
		DisabledSkills: []string{"quick-note"},
	}
	customCatalog, err := svc.ListSkills(wsDir, customCfg)
	if err != nil {
		t.Fatalf("ListSkills with customCfg failed: %v", err)
	}
	// shared-skill (project) + linked-agent-skill (user); quick-note is disabled!
	if len(customCatalog.Skills) != 2 {
		t.Fatalf("expected 2 enabled skills with quick-note disabled, got %#v", customCatalog.Skills)
	}
	if _, rErr := svc.ReadSkill("quick-note", wsDir, customCfg); rErr == nil {
		t.Fatalf("expected ReadSkill to reject disabled skill quick-note")
	}

	// InspectSkills should report directories, shadowed shared-skill in ~/.vela/skills, and disabled quick-note.
	insp, err := svc.InspectSkills(wsDir, customCfg)
	if err != nil {
		t.Fatalf("InspectSkills failed: %v", err)
	}
	if len(insp.Directories) != 4 {
		t.Fatalf("expected 4 directory reports, got %d", len(insp.Directories))
	}
	if insp.ActiveCount != 2 {
		t.Fatalf("expected ActiveCount=2, got %d", insp.ActiveCount)
	}
	// Last directory (~/.vela/skills) should mark shared-skill as overridden by .agents/skills and quick-note as disabled.
	velaReport := insp.Directories[3]
	foundShadowed := false
	foundDisabled := false
	for _, entry := range velaReport.Skills {
		if entry.Name == "shared-skill" && entry.OverriddenBy == ".agents/skills" {
			foundShadowed = true
		}
		if entry.Name == "quick-note" && !entry.Enabled {
			foundDisabled = true
		}
	}
	if !foundShadowed || !foundDisabled {
		t.Fatalf("expected shadowed shared-skill and disabled quick-note in ~/.vela/skills report, got %#v", velaReport)
	}
}

func TestReadFilePreviewKinds(t *testing.T) {
	dir := t.TempDir()
	svc := WorkspaceService{}

	htmlPath := filepath.Join(dir, "game.html")
	_ = os.WriteFile(htmlPath, []byte("<h1>Hello</h1>"), 0o644)
	pHTML, err := svc.ReadFilePreview(htmlPath)
	if err != nil || pHTML.Kind != "html" || pHTML.Content != "<h1>Hello</h1>" {
		t.Fatalf("unexpected html preview: err=%v preview=%#v", err, pHTML)
	}

	mdPath := filepath.Join(dir, "README.md")
	_ = os.WriteFile(mdPath, []byte("# Title\n\nBody"), 0o644)
	pMD, err := svc.ReadFilePreview(mdPath)
	if err != nil || pMD.Kind != "markdown" {
		t.Fatalf("unexpected markdown preview: err=%v preview=%#v", err, pMD)
	}

	codePath := filepath.Join(dir, "main.go")
	_ = os.WriteFile(codePath, []byte("package main\n"), 0o644)
	pCode, err := svc.ReadFilePreview(codePath)
	if err != nil || pCode.Kind != "code" {
		t.Fatalf("unexpected code preview: err=%v preview=%#v", err, pCode)
	}

	binPath := filepath.Join(dir, "data.bin")
	_ = os.WriteFile(binPath, []byte{0x00, 0x01, 0x02, 0xff}, 0o644)
	pBin, err := svc.ReadFilePreview(binPath)
	if err != nil || pBin.Kind != "binary" || pBin.Content != "" {
		t.Fatalf("unexpected binary preview: err=%v preview=%#v", err, pBin)
	}
}

func TestSandboxedAutoAllowAndSessionApproval(t *testing.T) {
	withOSSandboxAvailable(t, true)

	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	// 1. In sandboxed workspace-write mode, default-allow all non-blacklisted workspace commands
	// including lsof/netstat, 2>/dev/null, ||, subshells, python3, pipelines, and dev commands.
	autoAllowed := []string{
		"go test ./...",
		"go build .",
		"go vet ./...",
		"tsc -p tsconfig.json",
		"oxlint src",
		"pnpm run build",
		"ls -la | wc -l",
		"git log -n 5 --oneline | head -n 3",
		"go test ./... 2>&1 | tail -20",
		"python3 -V 2>&1",
		"python3 -V && ls -la",
		"go vet ./... && go test ./... 2>&1 | tail -5",
		"ls -la; git status -s",
		"(lsof -nP -iTCP -sTCP:LISTEN 2>/dev/null | head -20) || netstat -an 2>/dev/null | head -20",
		"python3 -V; python3 -c \"import flask; print('flask', flask.__version__)\" 2>&1 | tail -2; ls",
	}
	for _, cmd := range autoAllowed {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "sb-auto-1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionWorkspaceWrite,
			Input:          map[string]any{"command": cmd},
		})
		if res.Decision != DecisionAllow {
			t.Errorf("expected sandboxed workspace-write allow for %q, got %#v", cmd, res)
		}
		if cmd == "go test ./..." {
			ro := ts.ClassifyToolCall(ToolExecuteRequest{
				SessionID:      "sb-auto-1",
				ToolName:       "bash",
				Workdir:        wsDir,
				PermissionMode: PermissionReadOnly,
				Input:          map[string]any{"command": cmd},
			})
			if ro.Decision != DecisionDeny {
				t.Errorf("expected read-only deny for %q even with sandbox, got %#v", cmd, ro)
			}
		}
	}

	// 2. Blacklisted destructive/network commands (including inside chains, subshells, and $()) require approval.
	mustAskOnce := []string{
		"rm -rf build",
		"git push origin main",
		"git reset --hard HEAD",
		"curl https://example.com",
		"npm publish",
		"go test ./... && git push",
		"ls -la && rm -rf build",
		"(echo ok) || curl https://example.com",
		"echo $(curl https://example.com)",
	}
	for _, cmd := range mustAskOnce {
		res := ts.ClassifyToolCall(ToolExecuteRequest{
			SessionID:      "sb-auto-1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionWorkspaceWrite,
			Input:          map[string]any{"command": cmd},
		})
		if res.Decision != DecisionAsk {
			t.Errorf("expected ask for blacklisted cmd %q, got %#v", cmd, res)
		}
		if res.SessionAllowKey != "" {
			t.Errorf("expected empty sessionAllowKey for %q, got %q", cmd, res.SessionAllowKey)
		}
	}

	// 3. Custom user blacklist rules are respected immediately.
	customHit := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:         "sb-custom-1",
		ToolName:          "bash",
		Workdir:           wsDir,
		PermissionMode:    PermissionWorkspaceWrite,
		ApprovalBlacklist: []string{"python3", "docker rm"},
		Input:             map[string]any{"command": "python3 app.py"},
	})
	if customHit.Decision != DecisionAsk {
		t.Fatalf("expected custom blacklist to require approval for python3, got %#v", customHit)
	}
	customMiss := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:         "sb-custom-1",
		ToolName:          "bash",
		Workdir:           wsDir,
		PermissionMode:    PermissionWorkspaceWrite,
		ApprovalBlacklist: []string{"python3"},
		Input:             map[string]any{"command": "curl https://example.com"},
	})
	if customMiss.Decision != DecisionAllow {
		t.Fatalf("expected custom blacklist without curl to allow curl, got %#v", customMiss)
	}

	// 4. When OS sandbox is unavailable (e.g. Windows fallback), non-read-only commands ask with sessionAllowKey.
	osSandboxAvailableFn = func() bool { return false }

	first := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:      "sess-a",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Input:          map[string]any{"command": "python3 -c 'print(1)'"},
	})
	if first.Decision != DecisionAsk || first.SessionAllowKey != "python3" {
		t.Fatalf("expected ask with sessionAllowKey=python3, got %#v", first)
	}

	execRes, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "sess-a",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		AllowSession:   true,
		Input:          map[string]any{"command": "python3 -c 'print(1)'"},
	})
	if err != nil || execRes.IsError {
		t.Fatalf("expected approved python3 execution to succeed: err=%v res=%#v", err, execRes)
	}

	second := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:      "sess-a",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Input:          map[string]any{"command": "python3 -c 'print(2)'"},
	})
	if second.Decision != DecisionAllow {
		t.Fatalf("expected session-allowed python3 in sess-a, got %#v", second)
	}

	otherSess := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:      "sess-b",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Input:          map[string]any{"command": "python3 -c 'print(2)'"},
	})
	if otherSess.Decision != DecisionAsk {
		t.Fatalf("expected sess-b to still ask, got %#v", otherSess)
	}

	if err := ts.ClearSessionApprovals("sess-a"); err != nil {
		t.Fatalf("ClearSessionApprovals failed: %v", err)
	}
	afterClear := ts.ClassifyToolCall(ToolExecuteRequest{
		SessionID:      "sess-a",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Input:          map[string]any{"command": "python3 -c 'print(3)'"},
	})
	if afterClear.Decision != DecisionAsk {
		t.Fatalf("expected sess-a to ask again after ClearSessionApprovals, got %#v", afterClear)
	}
}
