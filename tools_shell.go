package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	bashDefaultTimeoutMs       = 60_000
	bashMaxTimeoutMs           = 300_000
	bashMaxOutputBytes         = 64 * 1024
	maxProducedFilesPerCommand = 50
)

var sensitiveEnvPattern = regexp.MustCompile(`(?i)(KEY|PASSWORD|SECRET|TOKEN)`)

// sanitizeEnv removes parent environment variables matching KEY|PASSWORD|SECRET|TOKEN
// before launching child shell processes.
func sanitizeEnv(environ []string) []string {
	clean := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, hasEq := strings.Cut(entry, "=")
		if !hasEq {
			continue
		}
		if sensitiveEnvPattern.MatchString(key) {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

type fileStamp struct {
	size        int64
	modTimeNano int64
}

func snapshotDirFiles(dir string) map[string]fileStamp {
	snap := make(map[string]fileStamp)
	if dir == "" {
		return snap
	}
	gitignore := newGitignoreMatcher(dir)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir {
				if shouldSkipDir(path, d.Name()) || gitignore.isIgnored(path, true) {
					return filepath.SkipDir
				}
			}
			gitignore.loadDir(path)
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if gitignore.isIgnored(path, false) {
			return nil
		}
		rel, rErr := filepath.Rel(dir, path)
		if rErr != nil {
			return nil
		}
		if info, iErr := d.Info(); iErr == nil {
			snap[rel] = fileStamp{
				size:        info.Size(),
				modTimeNano: info.ModTime().UnixNano(),
			}
		}
		return nil
	})
	return snap
}

func diffSnapshots(dir string, before, after map[string]fileStamp) []string {
	var changed []string
	for rel, stamp := range after {
		prev, existed := before[rel]
		if !existed || prev.size != stamp.size || prev.modTimeNano != stamp.modTimeNano {
			changed = append(changed, filepath.Join(dir, rel))
		}
	}
	sort.Strings(changed)
	if len(changed) > maxProducedFilesPerCommand {
		changed = changed[:maxProducedFilesPerCommand]
	}
	return changed
}

func isDirEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) == 0
}

func truncateOutput(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	return s[:maxBytes], true
}

// executeBash implements bash(description?, command, workdir?, timeoutMs?) with:
//   - process-group isolation and full subtree cleanup on user cancel
//   - lightweight OS sandbox (macOS sandbox-exec / Linux bwrap) for read-only & workspace-write
//   - automatic promotion to a background job on timeout (with ring buffers & job_output/job_kill)
//   - sensitive environment variable stripping
//   - lazy scratch dir creation with post-exit empty-dir cleanup
//   - before/after file snapshot diff for uiMeta.producedFiles
//   - non-zero exit codes returned as normal (non-error) tool results
func (s *ToolService) executeBash(ctx context.Context, req ToolExecuteRequest) (ToolExecuteResult, error) {
	command := strings.TrimSpace(stringInput(req.Input, "command"))
	if command == "" {
		return toolErrorResult("Error: command is required", req.Workdir)
	}

	description := strings.TrimSpace(stringInput(req.Input, "description"))
	timeoutMs := intInput(req.Input, "timeoutMs", bashDefaultTimeoutMs)
	if timeoutMs <= 0 {
		timeoutMs = bashDefaultTimeoutMs
	}
	if timeoutMs > bashMaxTimeoutMs {
		timeoutMs = bashMaxTimeoutMs
	}

	basePerm := normalizePermissionMode(req.PermissionMode)
	perm, escalated, _, escErr := parseSandboxEscalation(req, basePerm)
	if escErr != "" {
		return toolErrorResult("Error: "+escErr, req.Workdir)
	}
	if escalated && !req.Approved {
		return toolErrorResult("Error: sandbox escalation requires explicit one-time user approval", req.Workdir)
	}
	inputWorkdir := strings.TrimSpace(stringInput(req.Input, "workdir"))
	reqWorkdir := strings.TrimSpace(req.Workdir)

	var cwd string
	createdScratchNow := false

	switch {
	case inputWorkdir != "":
		resolved, _, err := resolveAndValidatePath(inputWorkdir, reqWorkdir, true, true)
		if err != nil {
			return toolErrorResult("Error: "+err.Error(), reqWorkdir)
		}
		cwd = resolved
	case reqWorkdir != "":
		resolved, err := canonicalizePath(reqWorkdir)
		if err != nil {
			return toolErrorResult("Error: "+err.Error(), reqWorkdir)
		}
		cwd = resolved
	default:
		// No workdir bound yet: lazily prepare ~/.vela/scratch/<sessionId>/ for safe cwd.
		scratchDir, err := scratchDirForSession(req.SessionID)
		if err != nil {
			return toolErrorResult("Error: "+err.Error(), "")
		}
		if _, statErr := os.Lstat(scratchDir); os.IsNotExist(statErr) {
			createdScratchNow = true
		}
		if err := os.MkdirAll(scratchDir, 0o755); err != nil {
			return toolErrorResult(fmt.Sprintf("Error creating scratch directory: %v", err), "")
		}
		if canonical, cErr := filepath.EvalSymlinks(scratchDir); cErr == nil {
			cwd = canonical
		} else {
			cwd = scratchDir
		}
	}

	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return toolErrorResult(fmt.Sprintf("Error: workdir %q does not exist or is not a directory", cwd), reqWorkdir)
	}

	beforeSnap := snapshotDirFiles(cwd)

	spec := sandboxSpec{
		sessionID:       req.SessionID,
		mode:            perm,
		writableRoots:   collectSandboxWritableRoots(perm, cwd, reqWorkdir),
		workspaceRoots:  collectSandboxWorkspaceRoots(perm, cwd, reqWorkdir),
		deniedReadPaths: collectSensitiveReadDenyPaths(cwd, reqWorkdir),
	}
	prepared, err := prepareSandboxedShellCommand(command, spec, sanitizeEnv(os.Environ()))
	if err != nil {
		if createdScratchNow && isDirEmpty(cwd) {
			_ = os.Remove(cwd)
		}
		return toolErrorResult(fmt.Sprintf("Error preparing sandbox: %v", err), reqWorkdir)
	}
	cmd := prepared.cmd
	sandboxed := prepared.sandboxed
	cleanupSandbox := prepared.cleanup
	cmd.Dir = cwd
	configureProcessGroup(cmd)

	stdoutBuf := newRingBuffer(jobRingBufferBytes)
	stderrBuf := newRingBuffer(jobRingBufferBytes)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	startedAt := time.Now()
	releaseJob, err := startCommandWithProcessTree(cmd)
	if err != nil {
		if cleanupSandbox != nil {
			cleanupSandbox()
		}
		if createdScratchNow && isDirEmpty(cwd) {
			_ = os.Remove(cwd)
		}
		return toolErrorResult(fmt.Sprintf("Error starting command: %v", err), reqWorkdir)
	}
	cleanupAll := func() {
		if releaseJob != nil {
			releaseJob()
		}
		if cleanupSandbox != nil {
			cleanupSandbox()
		}
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()

	select {
	case waitErr := <-waitCh:
		cleanupAll()
		return finishForegroundBash(
			waitErr,
			false,
			cwd,
			createdScratchNow,
			beforeSnap,
			stdoutBuf,
			stderrBuf,
			sandboxed,
			perm,
		), nil

	case <-ctx.Done():
		killProcessTree(cmd)
		waitErr := <-waitCh
		cleanupAll()
		return finishForegroundBash(
			waitErr,
			true,
			cwd,
			createdScratchNow,
			beforeSnap,
			stdoutBuf,
			stderrBuf,
			sandboxed,
			perm,
		), nil

	case <-timer.C:
		// Promote long-running command to a background job instead of killing it.
		job := &backgroundJob{
			sessionID:         req.SessionID,
			command:           command,
			description:       description,
			workdir:           cwd,
			startedAt:         startedAt,
			cmd:               cmd,
			stdout:            stdoutBuf,
			stderr:            stderrBuf,
			done:              make(chan struct{}),
			sandboxed:         sandboxed,
			sandboxMode:       perm,
			sandboxCleanup:    cleanupAll,
			createdScratchNow: createdScratchNow,
			beforeSnap:        beforeSnap,
			status:            jobStatusRunning,
		}
		jobID := s.registerBackgroundJob(job)

		go func() {
			waitErr := <-waitCh
			job.finish(waitErr)
		}()

		stdoutRaw, stderrRaw, dropped := job.consumeDelta()
		stdoutStr, outTrunc := truncateOutput(strings.TrimRight(stdoutRaw, "\r\n"), bashMaxOutputBytes)
		stderrStr, errTrunc := truncateOutput(strings.TrimRight(stderrRaw, "\r\n"), bashMaxOutputBytes)
		truncated := dropped || outTrunc || errTrunc

		var out strings.Builder
		if stdoutStr != "" {
			out.WriteString(stdoutStr)
		}
		if stderrStr != "" {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString("[stderr]\n")
			out.WriteString(stderrStr)
		}
		if truncated {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString(fmt.Sprintf("[truncated: output exceeded %d KB limit]", bashMaxOutputBytes/1024))
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf(
			"[command still running after %dms; moved to background job: %s. Use job_output(job_id=%q) to inspect output or job_kill(job_id=%q) to terminate it.]",
			timeoutMs, jobID, jobID, jobID,
		))

		assignedScratch := ""
		if createdScratchNow {
			assignedScratch = cwd
		}

		meta := map[string]any{
			"backgroundJobId": jobID,
			"jobStatus":       jobStatusRunning,
			"truncated":       truncated,
			"workdir":         cwd,
			"sandboxed":       sandboxed,
		}
		if sandboxed {
			meta["sandboxMode"] = perm
		}

		return ToolExecuteResult{
			ModelContent:    out.String(),
			IsError:         false,
			AssignedScratch: assignedScratch,
			UIMeta:          meta,
		}, nil
	}
}

func finishForegroundBash(
	waitErr error,
	canceled bool,
	cwd string,
	createdScratchNow bool,
	beforeSnap map[string]fileStamp,
	stdoutBuf *ringBuffer,
	stderrBuf *ringBuffer,
	sandboxed bool,
	perm string,
) ToolExecuteResult {
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	// Post-exit scratch cleanup & snapshot diff:
	// Only remove cwd if it was newly lazily created for this call AND remains completely empty.
	assignedScratch := ""
	var producedFiles []string
	effectiveWorkdir := cwd
	if createdScratchNow && isDirEmpty(cwd) {
		_ = os.Remove(cwd)
		effectiveWorkdir = ""
	} else {
		if createdScratchNow {
			assignedScratch = cwd
		}
		afterSnap := snapshotDirFiles(cwd)
		producedFiles = diffSnapshots(cwd, beforeSnap, afterSnap)
	}

	stdoutRaw, _, outDropped := stdoutBuf.readFrom(0)
	stderrRaw, _, errDropped := stderrBuf.readFrom(0)
	stdoutStr, outTrunc := truncateOutput(strings.TrimRight(stdoutRaw, "\r\n"), bashMaxOutputBytes)
	stderrStr, errTrunc := truncateOutput(strings.TrimRight(stderrRaw, "\r\n"), bashMaxOutputBytes)
	truncated := outDropped || errDropped || outTrunc || errTrunc

	var out strings.Builder
	if stdoutStr != "" {
		out.WriteString(stdoutStr)
	}
	if stderrStr != "" {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString("[stderr]\n")
		out.WriteString(stderrStr)
	}
	if truncated {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf("[truncated: output exceeded %d KB limit]", bashMaxOutputBytes/1024))
	}
	if canceled {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString("[cancelled by user]")
	}
	if exitCode != 0 {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf("[exit code: %d]", exitCode))
	}
	if sandboxed && !canceled && exitCode != 0 && looksLikeSandboxDenial(stderrStr) {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf("[sandbox: kernel blocked a write outside allowed roots under %s mode]", perm))
		if hint := sandboxEscalationHint(perm); hint != "" {
			out.WriteString("\n")
			out.WriteString(hint)
		}
	}
	if out.Len() == 0 {
		out.WriteString("(no output)")
	}

	meta := map[string]any{
		"exitCode":  exitCode,
		"truncated": truncated,
		"sandboxed": sandboxed,
	}
	if sandboxed {
		meta["sandboxMode"] = perm
	}
	if effectiveWorkdir != "" {
		meta["workdir"] = effectiveWorkdir
	}
	if len(producedFiles) > 0 {
		meta["producedFiles"] = producedFiles
	}

	return ToolExecuteResult{
		ModelContent:    out.String(),
		IsError:         canceled,
		AssignedScratch: assignedScratch,
		UIMeta:          meta,
	}
}
