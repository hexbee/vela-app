package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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

// truncateOutput retains the tail of s up to maxBytes on a valid UTF-8 boundary,
// preserving trailing progress lines, listening URLs, and error diagnostics.
func truncateOutput(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:], true
}

// executeBash implements bash(description?, command, workdir?, timeoutMs?, run_in_background?) with:
//   - job-first registration so live stdout/stderr stream immediately and users can detach to background on demand
//   - explicit run_in_background=true support for immediate non-blocking starts (servers, long downloads)
//   - process-group isolation and full subtree cleanup on user cancel
//   - lightweight OS sandbox (macOS sandbox-exec / Linux bwrap / Windows AppContainer) for read-only & workspace-write
//   - automatic promotion to a background job on timeout (with ring buffers & job_output/job_list/job_kill)
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
	runInBackground := boolInput(req.Input, "run_in_background")
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

	// Job-first registration: register every launched bash command immediately so
	// live output streams to the UI and the user can detach it to background at any time.
	job := &backgroundJob{
		sessionID:         req.SessionID,
		toolUseID:         strings.TrimSpace(req.ToolUseID),
		command:           command,
		description:       description,
		workdir:           cwd,
		startedAt:         startedAt,
		cmd:               cmd,
		stdout:            stdoutBuf,
		stderr:            stderrBuf,
		done:              make(chan struct{}),
		detachCh:          make(chan struct{}),
		sandboxed:         sandboxed,
		sandboxMode:       perm,
		sandboxCleanup:    cleanupAll,
		createdScratchNow: createdScratchNow,
		beforeSnap:        beforeSnap,
		status:            jobStatusRunning,
		exposedToModel:    runInBackground,
		streamDone:        make(chan struct{}),
	}
	jobID := s.registerBackgroundJob(job)

	go func() {
		waitErr := cmd.Wait()
		job.finish(waitErr)
		job.waitStreamDone()
		s.maybeEmitJobSettled(job)
		s.pruneFinishedJobs(job.sessionID)
	}()
	go s.streamJobOutput(job)

	if runInBackground {
		assignedScratch := ""
		if createdScratchNow {
			assignedScratch = cwd
		}
		meta := map[string]any{
			"backgroundJobId": jobID,
			"jobStatus":       jobStatusRunning,
			"workdir":         cwd,
			"sandboxed":       sandboxed,
		}
		if sandboxed {
			meta["sandboxMode"] = perm
		}
		return ToolExecuteResult{
			ModelContent: fmt.Sprintf(
				"Started background job: %s. Use job_output(job_id=%q) to inspect output, job_list() to check status, or job_kill(job_id=%q) to stop it.",
				jobID, jobID, jobID,
			),
			IsError:         false,
			AssignedScratch: assignedScratch,
			UIMeta:          meta,
		}, nil
	}

	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-job.done:
		s.unregisterBackgroundJob(jobID)
		return finishForegroundBash(job, false), nil

	case <-ctx.Done():
		job.kill()
		<-job.done
		s.unregisterBackgroundJob(jobID)
		return finishForegroundBash(job, true), nil

	case <-job.detachCh:
		return s.promoteRunningJobResult(job, true, timeoutMs), nil

	case <-timer.C:
		return s.promoteRunningJobResult(job, false, timeoutMs), nil
	}
}

func (s *ToolService) promoteRunningJobResult(job *backgroundJob, byUser bool, timeoutMs int) ToolExecuteResult {
	alreadySettled := job.markExposedToModel()
	if alreadySettled {
		job.waitStreamDone()
		s.maybeEmitJobSettled(job)
	}

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
		out.WriteString(fmt.Sprintf("[truncated: output exceeded %d KB limit; kept tail]", bashMaxOutputBytes/1024))
	}
	if out.Len() > 0 {
		out.WriteString("\n")
	}
	if byUser {
		out.WriteString(fmt.Sprintf(
			"[moved to background by user as %s. Do NOT wait for completion or call job_output in this turn — reply to the user immediately and let the command keep running in the background. The user can monitor live progress in the background jobs tray and trigger a follow-up once it finishes.]",
			job.id,
		))
	} else {
		out.WriteString(fmt.Sprintf(
			"[command still running after %dms; moved to background job: %s. Do not busy-poll job_output; reply to the user or inspect later with job_output(job_id=%q) / job_kill(job_id=%q).]",
			timeoutMs, job.id, job.id, job.id,
		))
	}

	assignedScratch := ""
	if job.createdScratchNow {
		assignedScratch = job.workdir
	}

	job.mu.Lock()
	status := job.status
	exitCode := job.exitCode
	job.mu.Unlock()

	meta := map[string]any{
		"backgroundJobId": job.id,
		"jobStatus":       status,
		"truncated":       truncated,
		"workdir":         job.workdir,
		"sandboxed":       job.sandboxed,
	}
	if job.sandboxed {
		meta["sandboxMode"] = job.sandboxMode
	}
	if status != jobStatusRunning {
		meta["exitCode"] = exitCode
	}

	return ToolExecuteResult{
		ModelContent:    out.String(),
		IsError:         false,
		AssignedScratch: assignedScratch,
		UIMeta:          meta,
	}
}

func finishForegroundBash(job *backgroundJob, canceled bool) ToolExecuteResult {
	job.mu.Lock()
	exitCode := job.exitCode
	producedFiles := append([]string(nil), job.producedFiles...)
	job.mu.Unlock()

	// Clean up a newly lazily-created empty scratch directory strictly on the
	// foreground-completion path so it can never race with background promotion.
	assignedScratch := ""
	effectiveWorkdir := job.workdir
	if job.createdScratchNow {
		if isDirEmpty(job.workdir) {
			_ = os.Remove(job.workdir)
			effectiveWorkdir = ""
		} else {
			assignedScratch = job.workdir
		}
	}

	stdoutRaw, _, outDropped := job.stdout.readFrom(0)
	stderrRaw, _, errDropped := job.stderr.readFrom(0)
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
		out.WriteString(fmt.Sprintf("[truncated: output exceeded %d KB limit; kept tail]", bashMaxOutputBytes/1024))
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
	if job.sandboxed && !canceled && exitCode != 0 && looksLikeSandboxDenial(stderrStr) {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf("[sandbox: kernel blocked a write outside allowed roots under %s mode]", job.sandboxMode))
		if hint := sandboxEscalationHint(job.sandboxMode); hint != "" {
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
		"sandboxed": job.sandboxed,
	}
	if job.sandboxed {
		meta["sandboxMode"] = job.sandboxMode
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
