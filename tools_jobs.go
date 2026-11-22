package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	jobRingBufferBytes = 256 * 1024
	jobMaxWaitMs       = 30_000
	jobStatusRunning   = "running"
	jobStatusCompleted = "completed"
	jobStatusKilled    = "killed"
)

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
	id          string
	sessionID   string
	command     string
	description string
	workdir     string
	startedAt   time.Time
	cmd         *exec.Cmd
	stdout      *ringBuffer
	stderr      *ringBuffer
	done           chan struct{}
	sandboxed      bool
	sandboxMode    string
	sandboxCleanup func()

	createdScratchNow bool
	beforeSnap        map[string]fileStamp

	mu               sync.Mutex
	status           string
	exitCode         int
	endedAt          time.Time
	stdoutCursor     int64
	stderrCursor     int64
	reportedProduced bool
	producedFiles    []string
}

func (j *backgroundJob) info() BackgroundJobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	info := BackgroundJobInfo{
		ID:          j.id,
		SessionID:   j.sessionID,
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
		if j.createdScratchNow && isDirEmpty(j.workdir) {
			_ = os.Remove(j.workdir)
		} else {
			afterSnap := snapshotDirFiles(j.workdir)
			produced = diffSnapshots(j.workdir, j.beforeSnap, afterSnap)
		}
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

func (s *ToolService) getBackgroundJob(jobID string) *backgroundJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		return nil
	}
	return s.jobs[jobID]
}

// ListBackgroundJobs returns all background bash jobs for a session (or all sessions if sessionID is empty).
func (s *ToolService) ListBackgroundJobs(sessionID string) []BackgroundJobInfo {
	s.mu.Lock()
	jobs := make([]*backgroundJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		if sessionID == "" || j.sessionID == sessionID {
			jobs = append(jobs, j)
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
	job := s.getBackgroundJob(strings.TrimSpace(jobID))
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
		outCur := job.stdoutCursor
		errCur := job.stderrCursor
		running := job.status == jobStatusRunning
		job.mu.Unlock()

		if running {
			outCh, hasOut := job.stdout.waitChannel(outCur)
			errCh, hasErr := job.stderr.waitChannel(errCur)
			if !hasOut && !hasErr {
				timer := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
				select {
				case <-outCh:
				case <-errCh:
				case <-job.done:
				case <-ctx.Done():
				case <-timer.C:
				}
				timer.Stop()
			}
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
