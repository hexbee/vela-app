package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo"
)

const (
	jobRingBufferBytes        = 256 * 1024
	jobMaxWaitMs              = 30_000
	jobStreamFlushInterval    = 120 * time.Millisecond
	jobStreamMaxChunkBytes    = 16 * 1024
	jobSettledTailBytes       = 2048
	maxFinishedJobsPerSession = 20
	jobStatusRunning          = "running"
	jobStatusCompleted        = "completed"
	jobStatusKilled           = "killed"
)

// JobOutputDelta carries live incremental stdout/stderr output for a running or finishing bash job.
type JobOutputDelta struct {
	JobID     string `json:"jobId"`
	SessionID string `json:"sessionId"`
	ToolUseID string `json:"toolUseId,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Status    string `json:"status"` // "running" | "completed" | "killed"
}

// JobSettledNotice notifies the frontend when a background bash job completes or is killed.
type JobSettledNotice struct {
	JobID         string   `json:"jobId"`
	SessionID     string   `json:"sessionId"`
	ToolUseID     string   `json:"toolUseId,omitempty"`
	Command       string   `json:"command"`
	Description   string   `json:"description,omitempty"`
	Status        string   `json:"status"` // "completed" | "killed"
	ExitCode      int      `json:"exitCode"`
	OutputTail    string   `json:"outputTail,omitempty"`
	ProducedFiles []string `json:"producedFiles,omitempty"`
	EndedAt       int64    `json:"endedAt"`
}

// JobOutputEvent pushes live bash stdout/stderr deltas and status updates to the frontend.
var JobOutputEvent = mygo.NewEvent[JobOutputDelta]("job:output")

// JobSettledEvent notifies the frontend when a background bash job finishes or is terminated.
var JobSettledEvent = mygo.NewEvent[JobSettledNotice]("job:settled")

// ringBuffer is a thread-safe bounded circular byte buffer that tracks cumulative
// written byte offsets so multiple reads can consume incremental deltas safely.
type ringBuffer struct {
	mu           sync.Mutex
	buf          []byte
	limit        int
	totalWritten int64
	notifyCh     chan struct{}
}

func newRingBuffer(limit int) *ringBuffer {
	if limit <= 0 {
		limit = jobRingBufferBytes
	}
	return &ringBuffer{
		limit:    limit,
		notifyCh: make(chan struct{}),
	}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.totalWritten += int64(len(p))
	if len(p) >= r.limit {
		r.buf = append(r.buf[:0], p[len(p)-r.limit:]...)
	} else {
		overflow := len(r.buf) + len(p) - r.limit
		if overflow > 0 {
			copy(r.buf, r.buf[overflow:])
			r.buf = r.buf[:len(r.buf)-overflow]
		}
		r.buf = append(r.buf, p...)
	}

	close(r.notifyCh)
	r.notifyCh = make(chan struct{})
	return len(p), nil
}

// readFrom returns bytes written since byte offset `from`, the new offset, and whether
// older bytes were overwritten by the ring buffer before this read.
func (r *ringBuffer) readFrom(from int64) (text string, nextOffset int64, dropped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	nextOffset = r.totalWritten
	baseOffset := r.totalWritten - int64(len(r.buf))
	if from < baseOffset {
		dropped = true
		from = baseOffset
	}
	if from >= r.totalWritten {
		return "", nextOffset, dropped
	}
	startIdx := int(from - baseOffset)
	return string(r.buf[startIdx:]), nextOffset, dropped
}

// readUTF8SafeFrom reads bytes since `from` and, when `final` is false, holds back
// any trailing incomplete multi-byte UTF-8 sequence so streaming chunks never emit U+FFFD.
func (r *ringBuffer) readUTF8SafeFrom(from int64, final bool) (text string, nextOffset int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	nextOffset = r.totalWritten
	baseOffset := r.totalWritten - int64(len(r.buf))
	if from < baseOffset {
		from = baseOffset
	}
	if from >= r.totalWritten {
		return "", nextOffset
	}
	chunk := r.buf[int(from-baseOffset):]
	if !final {
		hold := trailingIncompleteUTF8Bytes(chunk)
		if hold > 0 {
			chunk = chunk[:len(chunk)-hold]
			nextOffset -= int64(hold)
		}
	}
	if len(chunk) == 0 {
		return "", nextOffset
	}
	return string(chunk), nextOffset
}

func trailingIncompleteUTF8Bytes(b []byte) int {
	n := len(b)
	if n == 0 {
		return 0
	}
	// Walk back at most 3 bytes to find the start of the final rune.
	for back := 1; back <= 3 && back <= n; back++ {
		idx := n - back
		if utf8.RuneStart(b[idx]) {
			if utf8.FullRune(b[idx:]) {
				return 0
			}
			return back
		}
	}
	return 0
}

func (r *ringBuffer) waitChannel(from int64) (<-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.totalWritten > from {
		return nil, true
	}
	return r.notifyCh, false
}

// BackgroundJobInfo is a UI-friendly snapshot of a background bash job.
type BackgroundJobInfo struct {
	ID          string `json:"id"`
	SessionID   string `json:"sessionId"`
	ToolUseID   string `json:"toolUseId,omitempty"`
	Command     string `json:"command"`
	Description string `json:"description"`
	Workdir     string `json:"workdir"`
	Status      string `json:"status"` // "running" | "completed" | "killed"
	ExitCode    int    `json:"exitCode"`
	StartedAt   int64  `json:"startedAt"`
	EndedAt     int64  `json:"endedAt,omitempty"`
	Sandboxed   bool   `json:"sandboxed"`
	SandboxMode string `json:"sandboxMode,omitempty"`
}

type backgroundJob struct {
	id             string
	sessionID      string
	toolUseID      string
	command        string
	description    string
	workdir        string
	startedAt      time.Time
	cmd            *exec.Cmd
	stdout         *ringBuffer
	stderr         *ringBuffer
	done           chan struct{}
	detachCh       chan struct{}
	detachOnce     sync.Once
	streamDone     chan struct{}
	sandboxed      bool
	sandboxMode    string
	sandboxCleanup func()

	createdScratchNow bool
	beforeSnap        map[string]fileStamp

	mu                 sync.Mutex
	status             string
	exitCode           int
	endedAt            time.Time
	stdoutCursor       int64
	stderrCursor       int64
	streamStdoutCursor int64
	streamStderrCursor int64
	reportedProduced   bool
	producedFiles      []string
	exposedToModel     bool
	settledEmitted     bool
}

func (j *backgroundJob) info() BackgroundJobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	info := BackgroundJobInfo{
		ID:          j.id,
		SessionID:   j.sessionID,
		ToolUseID:   j.toolUseID,
		Command:     j.command,
		Description: j.description,
		Workdir:     j.workdir,
		Status:      j.status,
		ExitCode:    j.exitCode,
		StartedAt:   j.startedAt.UnixMilli(),
		Sandboxed:   j.sandboxed,
		SandboxMode: j.sandboxMode,
	}
	if !j.endedAt.IsZero() {
		info.EndedAt = j.endedAt.UnixMilli()
	}
	return info
}

func (j *backgroundJob) requestDetach() {
	if j.detachCh == nil {
		return
	}
	j.detachOnce.Do(func() {
		close(j.detachCh)
	})
}

func (j *backgroundJob) markExposedToModel() (alreadySettled bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.exposedToModel = true
	return j.status != jobStatusRunning
}

// waitStreamDone blocks until the live-output goroutine has emitted its final
// delta, guaranteeing job:settled always arrives after the last job:output chunk.
// It returns immediately for jobs that never started streaming.
func (j *backgroundJob) waitStreamDone() {
	if j.streamDone == nil {
		return
	}
	<-j.streamDone
}

func (j *backgroundJob) consumeDelta() (stdoutStr, stderrStr string, dropped bool) {
	j.mu.Lock()
	outCur := j.stdoutCursor
	errCur := j.stderrCursor
	j.mu.Unlock()

	outText, nextOut, outDropped := j.stdout.readFrom(outCur)
	errText, nextErr, errDropped := j.stderr.readFrom(errCur)

	j.mu.Lock()
	j.stdoutCursor = nextOut
	j.stderrCursor = nextErr
	j.mu.Unlock()

	return outText, errText, outDropped || errDropped
}

func (j *backgroundJob) consumeStreamDelta(final bool) (stdoutStr, stderrStr string, status string) {
	j.mu.Lock()
	outCur := j.streamStdoutCursor
	errCur := j.streamStderrCursor
	j.mu.Unlock()

	outText, nextOut := j.stdout.readUTF8SafeFrom(outCur, final)
	errText, nextErr := j.stderr.readUTF8SafeFrom(errCur, final)

	j.mu.Lock()
	j.streamStdoutCursor = nextOut
	j.streamStderrCursor = nextErr
	status = j.status
	j.mu.Unlock()

	return outText, errText, status
}

func (j *backgroundJob) buildSettledNotice() (JobSettledNotice, bool) {
	j.mu.Lock()
	if j.settledEmitted || !j.exposedToModel || j.status == jobStatusRunning {
		j.mu.Unlock()
		return JobSettledNotice{}, false
	}
	j.settledEmitted = true
	status := j.status
	exitCode := j.exitCode
	endedAt := j.endedAt.UnixMilli()
	produced := append([]string(nil), j.producedFiles...)
	j.mu.Unlock()

	outRaw, _, _ := j.stdout.readFrom(0)
	errRaw, _, _ := j.stderr.readFrom(0)
	var combined strings.Builder
	if outTrim := strings.TrimRight(outRaw, "\r\n"); outTrim != "" {
		combined.WriteString(outTrim)
	}
	if errTrim := strings.TrimRight(errRaw, "\r\n"); errTrim != "" {
		if combined.Len() > 0 {
			combined.WriteString("\n")
		}
		combined.WriteString("[stderr]\n")
		combined.WriteString(errTrim)
	}
	tail, _ := truncateOutput(combined.String(), jobSettledTailBytes)

	return JobSettledNotice{
		JobID:         j.id,
		SessionID:     j.sessionID,
		ToolUseID:     j.toolUseID,
		Command:       j.command,
		Description:   j.description,
		Status:        status,
		ExitCode:      exitCode,
		OutputTail:    tail,
		ProducedFiles: produced,
		EndedAt:       endedAt,
	}, true
}

func (j *backgroundJob) finish(waitErr error) {
	if j.sandboxCleanup != nil {
		j.sandboxCleanup()
		j.sandboxCleanup = nil
	}
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	var produced []string
	if j.workdir != "" {
		afterSnap := snapshotDirFiles(j.workdir)
		produced = diffSnapshots(j.workdir, j.beforeSnap, afterSnap)
	}

	j.mu.Lock()
	if j.status == jobStatusRunning {
		j.status = jobStatusCompleted
	}
	j.exitCode = exitCode
	j.endedAt = time.Now()
	j.producedFiles = produced
	j.mu.Unlock()

	close(j.done)
}

func (j *backgroundJob) kill() {
	j.mu.Lock()
	wasRunning := j.status == jobStatusRunning
	if wasRunning {
		j.status = jobStatusKilled
	}
	j.mu.Unlock()

	if wasRunning {
		killProcessTree(j.cmd)
	}
}

func (s *ToolService) emitJobOutput(delta JobOutputDelta) {
	s.mu.Lock()
	fn := s.emitOutputFn
	s.mu.Unlock()
	if fn != nil {
		fn(delta)
		return
	}
	if win := getMainWindow(); win != nil {
		_ = JobOutputEvent.Emit(win, delta)
	}
}

func (s *ToolService) emitJobSettled(notice JobSettledNotice) {
	s.mu.Lock()
	fn := s.emitSettledFn
	s.mu.Unlock()
	if fn != nil {
		fn(notice)
		return
	}
	if win := getMainWindow(); win != nil {
		_ = JobSettledEvent.Emit(win, notice)
	}
}

func (s *ToolService) maybeEmitJobSettled(job *backgroundJob) {
	if notice, ok := job.buildSettledNotice(); ok {
		s.emitJobSettled(notice)
	}
}

func (s *ToolService) streamJobOutput(job *backgroundJob) {
	if job.streamDone != nil {
		defer close(job.streamDone)
	}

	// Skip emitting a stale "running" announcement if a fast foreground command already exited.
	select {
	case <-job.done:
		job.mu.Lock()
		exposed := job.exposedToModel
		job.mu.Unlock()
		if !exposed {
			return
		}
	default:
	}

	// Immediately announce job identity so the UI can bind toolUseId -> jobId and offer "Move to background".
	s.emitJobOutput(JobOutputDelta{
		JobID:     job.id,
		SessionID: job.sessionID,
		ToolUseID: job.toolUseID,
		Status:    jobStatusRunning,
	})

	flush := func(forceStatus bool) {
		outStr, errStr, status := job.consumeStreamDelta(forceStatus)
		if outStr == "" && errStr == "" && !forceStatus {
			return
		}
		if len(outStr) > jobStreamMaxChunkBytes {
			outStr, _ = truncateOutput(outStr, jobStreamMaxChunkBytes)
		}
		if len(errStr) > jobStreamMaxChunkBytes {
			errStr, _ = truncateOutput(errStr, jobStreamMaxChunkBytes)
		}
		s.emitJobOutput(JobOutputDelta{
			JobID:     job.id,
			SessionID: job.sessionID,
			ToolUseID: job.toolUseID,
			Stdout:    outStr,
			Stderr:    errStr,
			Status:    status,
		})
	}

	for {
		job.mu.Lock()
		outCur := job.streamStdoutCursor
		errCur := job.streamStderrCursor
		job.mu.Unlock()

		outCh, hasOut := job.stdout.waitChannel(outCur)
		errCh, hasErr := job.stderr.waitChannel(errCur)

		if !hasOut && !hasErr {
			select {
			case <-outCh:
			case <-errCh:
			case <-job.done:
				flush(true)
				return
			}
		}

		// Brief coalesce window to avoid flooding WebView IPC on chatty output.
		timer := time.NewTimer(jobStreamFlushInterval)
		select {
		case <-timer.C:
			flush(false)
		case <-job.done:
			timer.Stop()
			flush(true)
			return
		}
	}
}

func (s *ToolService) registerBackgroundJob(job *backgroundJob) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = make(map[string]*backgroundJob)
	}
	s.nextJobID++
	job.id = fmt.Sprintf("job-%d", s.nextJobID)
	s.jobs[job.id] = job
	return job.id
}

func (s *ToolService) unregisterBackgroundJob(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, jobID)
}

// pruneFinishedJobs evicts the oldest completed/killed background jobs for sessionID
// once the count exceeds maxFinishedJobsPerSession to bound ring-buffer memory.
func (s *ToolService) pruneFinishedJobs(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) <= maxFinishedJobsPerSession {
		return
	}
	type finishedEntry struct {
		id      string
		endedAt time.Time
	}
	var finished []finishedEntry
	for id, j := range s.jobs {
		if j.sessionID != sessionID {
			continue
		}
		j.mu.Lock()
		done := j.exposedToModel && j.status != jobStatusRunning
		ended := j.endedAt
		j.mu.Unlock()
		if done {
			finished = append(finished, finishedEntry{id: id, endedAt: ended})
		}
	}
	if len(finished) <= maxFinishedJobsPerSession {
		return
	}
	sort.Slice(finished, func(i, k int) bool {
		return finished[i].endedAt.After(finished[k].endedAt)
	})
	for _, stale := range finished[maxFinishedJobsPerSession:] {
		delete(s.jobs, stale.id)
	}
}

func (s *ToolService) getBackgroundJob(jobID string) *backgroundJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		return nil
	}
	return s.jobs[jobID]
}

func (s *ToolService) findBackgroundJob(target string) *backgroundJob {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		return nil
	}
	if j, ok := s.jobs[target]; ok {
		return j
	}
	var matched *backgroundJob
	for _, j := range s.jobs {
		if j.toolUseID == target {
			if matched == nil || j.startedAt.After(matched.startedAt) {
				matched = j
			}
		}
	}
	return matched
}

// PromoteBackgroundJob detaches a currently running foreground bash command (matched by
// job ID or toolUseId) into a background job immediately so the model can resume replying.
func (s *ToolService) PromoteBackgroundJob(target string) (BackgroundJobInfo, error) {
	job := s.findBackgroundJob(target)
	if job == nil {
		return BackgroundJobInfo{}, fmt.Errorf("running bash job %q not found", target)
	}
	job.mu.Lock()
	running := job.status == jobStatusRunning
	job.mu.Unlock()
	if !running {
		return job.info(), nil
	}
	job.requestDetach()
	return job.info(), nil
}

// ListBackgroundJobs returns all exposed background bash jobs for a session (or all sessions if sessionID is empty).
func (s *ToolService) ListBackgroundJobs(sessionID string) []BackgroundJobInfo {
	s.mu.Lock()
	jobs := make([]*backgroundJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		if sessionID == "" || j.sessionID == sessionID {
			j.mu.Lock()
			exposed := j.exposedToModel
			j.mu.Unlock()
			if exposed {
				jobs = append(jobs, j)
			}
		}
	}
	s.mu.Unlock()

	result := make([]BackgroundJobInfo, 0, len(jobs))
	for _, j := range jobs {
		result = append(result, j.info())
	}
	sort.Slice(result, func(i, k int) bool {
		return result[i].StartedAt > result[k].StartedAt
	})
	return result
}

// KillBackgroundJob terminates a running background bash job by ID.
func (s *ToolService) KillBackgroundJob(jobID string) error {
	job := s.findBackgroundJob(jobID)
	if job == nil {
		return fmt.Errorf("background job %q not found", jobID)
	}
	job.kill()
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
	}
	return nil
}

// KillSessionBackgroundJobs terminates all running background bash jobs belonging to
// sessionID, waits briefly for cleanup hooks, and removes all job records for that session.
func (s *ToolService) KillSessionBackgroundJobs(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}

	s.mu.Lock()
	var running []*backgroundJob
	for id, j := range s.jobs {
		if j.sessionID != sessionID {
			continue
		}
		j.mu.Lock()
		j.settledEmitted = true
		isRunning := j.status == jobStatusRunning
		j.mu.Unlock()
		if isRunning {
			running = append(running, j)
		}
		delete(s.jobs, id)
	}
	s.mu.Unlock()

	for _, j := range running {
		j.kill()
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, j := range running {
		rem := time.Until(deadline)
		if rem <= 0 {
			break
		}
		select {
		case <-j.done:
		case <-time.After(rem):
		}
	}
	return nil
}

// KillAllBackgroundJobs terminates all currently running background bash jobs
// across all sessions and waits briefly for their cleanup hooks to complete.
func (s *ToolService) KillAllBackgroundJobs() {
	s.mu.Lock()
	running := make([]*backgroundJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		j.mu.Lock()
		isRunning := j.status == jobStatusRunning
		j.mu.Unlock()
		if isRunning {
			running = append(running, j)
		}
	}
	s.mu.Unlock()

	for _, j := range running {
		j.kill()
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, j := range running {
		rem := time.Until(deadline)
		if rem <= 0 {
			break
		}
		select {
		case <-j.done:
		case <-time.After(rem):
		}
	}
}

func (s *ToolService) executeJobList(req ToolExecuteRequest) (ToolExecuteResult, error) {
	jobs := s.ListBackgroundJobs(req.SessionID)
	if len(jobs) == 0 {
		return ToolExecuteResult{
			ModelContent: "(no background jobs)",
			IsError:      false,
		}, nil
	}

	var out strings.Builder
	for i, j := range jobs {
		if i > 0 {
			out.WriteString("\n")
		}
		label := strings.TrimSpace(j.Description)
		if label == "" {
			label = j.Command
		}
		if j.Status != jobStatusRunning {
			out.WriteString(fmt.Sprintf("%s [%s | exit=%d] — %s", j.ID, j.Status, j.ExitCode, label))
		} else {
			out.WriteString(fmt.Sprintf("%s [%s] — %s", j.ID, j.Status, label))
		}
	}

	return ToolExecuteResult{
		ModelContent: out.String(),
		IsError:      false,
	}, nil
}

func (s *ToolService) executeJobOutput(ctx context.Context, req ToolExecuteRequest) (ToolExecuteResult, error) {
	jobID := strings.TrimSpace(stringInput(req.Input, "job_id"))
	if jobID == "" {
		return toolErrorResult("Error: job_id is required", req.Workdir)
	}
	job := s.getBackgroundJob(jobID)
	if job == nil {
		return toolErrorResult(fmt.Sprintf("Error: background job %q not found", jobID), req.Workdir)
	}
	if req.SessionID != "" && job.sessionID != "" && job.sessionID != req.SessionID {
		return toolErrorResult(fmt.Sprintf("Error: background job %q belongs to another session", jobID), req.Workdir)
	}

	waitMs := intInput(req.Input, "wait_ms", 0)
	if waitMs < 0 {
		waitMs = 0
	}
	if waitMs > jobMaxWaitMs {
		waitMs = jobMaxWaitMs
	}

	if waitMs > 0 {
		job.mu.Lock()
		running := job.status == jobStatusRunning
		job.mu.Unlock()

		if running {
			timer := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
			select {
			case <-job.done:
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
		}
	}

	stdoutStr, stderrStr, dropped := job.consumeDelta()
	stdoutStr = strings.TrimRight(stdoutStr, "\r\n")
	stderrStr = strings.TrimRight(stderrStr, "\r\n")

	job.mu.Lock()
	status := job.status
	exitCode := job.exitCode
	var produced []string
	if status != jobStatusRunning && !job.reportedProduced && len(job.producedFiles) > 0 {
		produced = append([]string(nil), job.producedFiles...)
		job.reportedProduced = true
	}
	job.mu.Unlock()

	var out strings.Builder
	if dropped {
		out.WriteString("[notice: older background output was truncated by ring buffer]\n")
	}
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
	if out.Len() == 0 {
		out.WriteString("(no new output)")
	}
	out.WriteString(fmt.Sprintf("\n[job: %s | status: %s]", job.id, status))
	if status != jobStatusRunning && exitCode != 0 {
		out.WriteString(fmt.Sprintf("\n[exit code: %d]", exitCode))
	}
	if job.sandboxed && status != jobStatusRunning && exitCode != 0 && looksLikeSandboxDenial(stderrStr) {
		out.WriteString(fmt.Sprintf("\n[sandbox: kernel blocked an operation outside allowed roots under %s mode]", job.sandboxMode))
		if hint := sandboxEscalationHint(job.sandboxMode); hint != "" {
			out.WriteString("\n")
			out.WriteString(hint)
		}
	}

	meta := map[string]any{
		"backgroundJobId": job.id,
		"jobStatus":       status,
		"truncated":       dropped,
		"sandboxed":       job.sandboxed,
	}
	if job.sandboxMode != "" {
		meta["sandboxMode"] = job.sandboxMode
	}
	if job.workdir != "" {
		meta["workdir"] = job.workdir
	}
	if status != jobStatusRunning {
		meta["exitCode"] = exitCode
	}
	if len(produced) > 0 {
		meta["producedFiles"] = produced
	}

	return ToolExecuteResult{
		ModelContent: out.String(),
		IsError:      false,
		UIMeta:       meta,
	}, nil
}

func (s *ToolService) executeJobKill(req ToolExecuteRequest) (ToolExecuteResult, error) {
	jobID := strings.TrimSpace(stringInput(req.Input, "job_id"))
	if jobID == "" {
		return toolErrorResult("Error: job_id is required", req.Workdir)
	}
	job := s.getBackgroundJob(jobID)
	if job == nil {
		return toolErrorResult(fmt.Sprintf("Error: background job %q not found", jobID), req.Workdir)
	}
	if req.SessionID != "" && job.sessionID != "" && job.sessionID != req.SessionID {
		return toolErrorResult(fmt.Sprintf("Error: background job %q belongs to another session", jobID), req.Workdir)
	}

	job.kill()
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
	}

	stdoutStr, stderrStr, dropped := job.consumeDelta()
	stdoutStr = strings.TrimRight(stdoutStr, "\r\n")
	stderrStr = strings.TrimRight(stderrStr, "\r\n")

	job.mu.Lock()
	status := job.status
	exitCode := job.exitCode
	job.mu.Unlock()

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
	if out.Len() > 0 {
		out.WriteString("\n")
	}
	out.WriteString(fmt.Sprintf("[job: %s | status: %s]", job.id, status))

	meta := map[string]any{
		"backgroundJobId": job.id,
		"jobStatus":       status,
		"exitCode":        exitCode,
		"truncated":       dropped,
	}
	if job.workdir != "" {
		meta["workdir"] = job.workdir
	}

	return ToolExecuteResult{
		ModelContent: out.String(),
		IsError:      false,
		UIMeta:       meta,
	}, nil
}
