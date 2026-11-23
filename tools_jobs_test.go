package main

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBashRunInBackgroundImmediateAndSettledEvent(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	var (
		mu      sync.Mutex
		outputs []JobOutputDelta
		settled []JobSettledNotice
	)
	settledCh := make(chan JobSettledNotice, 2)
	ts.emitOutputFn = func(d JobOutputDelta) {
		mu.Lock()
		outputs = append(outputs, d)
		mu.Unlock()
	}
	ts.emitSettledFn = func(n JobSettledNotice) {
		mu.Lock()
		settled = append(settled, n)
		mu.Unlock()
		select {
		case settledCh <- n:
		default:
		}
	}

	start := time.Now()
	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "bg-explicit-1",
		ToolUseID:      "toolu_bg_1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input: map[string]any{
			"description":       "explicit background command",
			"command":           "echo alpha; sleep 0.2; echo beta",
			"run_in_background": true,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("run_in_background failed: err=%v res=%#v", err, res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("run_in_background should return immediately, took %v", time.Since(start))
	}
	jobID, _ := res.UIMeta["backgroundJobId"].(string)
	if jobID == "" {
		t.Fatalf("expected backgroundJobId, got %#v", res.UIMeta)
	}

	// job_list should report the running job immediately.
	listRes, lErr := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "bg-explicit-1",
		ToolName:       "job_list",
		Workdir:        wsDir,
		PermissionMode: PermissionReadOnly,
		Input:          map[string]any{},
	})
	if lErr != nil || listRes.IsError || !strings.Contains(listRes.ModelContent, jobID) {
		t.Fatalf("expected job_list to include %q, err=%v res=%#v", jobID, lErr, listRes)
	}

	// Wait for job:settled event.
	select {
	case notice := <-settledCh:
		if notice.JobID != jobID || notice.ToolUseID != "toolu_bg_1" || notice.Status != jobStatusCompleted || notice.ExitCode != 0 {
			t.Fatalf("unexpected settled notice: %#v", notice)
		}
		if !strings.Contains(notice.OutputTail, "beta") {
			t.Fatalf("expected beta in settled OutputTail, got %q", notice.OutputTail)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("timed out waiting for job:settled event")
	}

	// Verify live job:output events were emitted with toolUseId correlation.
	mu.Lock()
	var combinedOut strings.Builder
	sawInitial := false
	for _, ev := range outputs {
		if ev.JobID == jobID && ev.ToolUseID == "toolu_bg_1" {
			sawInitial = true
			combinedOut.WriteString(ev.Stdout)
		}
	}
	settledCount := len(settled)
	mu.Unlock()

	if !sawInitial {
		t.Fatalf("expected job:output events correlated with toolu_bg_1")
	}
	if !strings.Contains(combinedOut.String(), "alpha") || !strings.Contains(combinedOut.String(), "beta") {
		t.Fatalf("expected streamed stdout to contain alpha and beta, got %q", combinedOut.String())
	}
	if settledCount != 1 {
		t.Fatalf("expected exactly 1 settled notice, got %d", settledCount)
	}
}

func TestBashPromoteForegroundJobByUser(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	startedCh := make(chan JobOutputDelta, 4)
	settledCh := make(chan JobSettledNotice, 2)
	ts.emitOutputFn = func(d JobOutputDelta) {
		if d.ToolUseID == "toolu_detach_1" {
			select {
			case startedCh <- d:
			default:
			}
		}
	}
	ts.emitSettledFn = func(n JobSettledNotice) {
		select {
		case settledCh <- n:
		default:
		}
	}

	resultCh := make(chan ToolExecuteResult, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := ts.ExecuteTool(ToolExecuteRequest{
			SessionID:      "detach-sess-1",
			ToolUseID:      "toolu_detach_1",
			ToolName:       "bash",
			Workdir:        wsDir,
			PermissionMode: PermissionWorkspaceWrite,
			Approved:       true,
			Input: map[string]any{
				"description": "long foreground command",
				"command":     "echo before-detach; sleep 0.5; echo after-detach",
				"timeoutMs":   30_000,
			},
		})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- r
	}()

	// Wait until the live stream announces the running job.
	var announced JobOutputDelta
	select {
	case announced = <-startedCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for initial job:output announcement")
	}
	if announced.JobID == "" {
		t.Fatalf("expected non-empty JobID in initial job:output event")
	}

	// User clicks "Move to background" using toolUseId.
	promoteStart := time.Now()
	info, pErr := ts.PromoteBackgroundJob("toolu_detach_1")
	if pErr != nil {
		t.Fatalf("PromoteBackgroundJob failed: %v", pErr)
	}
	if info.ID != announced.JobID {
		t.Fatalf("expected promoted job %q, got %#v", announced.JobID, info)
	}

	select {
	case err := <-errCh:
		t.Fatalf("ExecuteTool returned error after promote: %v", err)
	case res := <-resultCh:
		if time.Since(promoteStart) > 2*time.Second {
			t.Fatalf("ExecuteTool should return promptly after PromoteBackgroundJob, took %v", time.Since(promoteStart))
		}
		if res.IsError {
			t.Fatalf("promoted result should not be error: %#v", res)
		}
		if !strings.Contains(res.ModelContent, "moved to background by user") {
			t.Fatalf("expected user-detach notice in ModelContent, got:\n%s", res.ModelContent)
		}
		if gotID, _ := res.UIMeta["backgroundJobId"].(string); gotID != announced.JobID {
			t.Fatalf("expected backgroundJobId=%q, got %#v", announced.JobID, res.UIMeta)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for ExecuteTool to return after PromoteBackgroundJob")
	}

	// Background process should finish cleanly and emit job:settled.
	select {
	case notice := <-settledCh:
		if notice.JobID != announced.JobID || notice.Status != jobStatusCompleted {
			t.Fatalf("unexpected settled notice after promote: %#v", notice)
		}
		if !strings.Contains(notice.OutputTail, "after-detach") {
			t.Fatalf("expected after-detach in OutputTail, got %q", notice.OutputTail)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("timed out waiting for settled event after user promote")
	}
}

func TestTruncateOutputKeepsTailAndValidUTF8(t *testing.T) {
	raw := "HEAD-" + strings.Repeat("中", 20) + "-TAIL"
	got, truncated := truncateOutput(raw, 10)
	if !truncated {
		t.Fatalf("expected truncated=true")
	}
	if !strings.HasSuffix(got, "-TAIL") {
		t.Fatalf("expected tail to be preserved, got %q", got)
	}
	if len(got) > 10 {
		t.Fatalf("expected len <= 10, got %d (%q)", len(got), got)
	}
}

func TestBashBackgroundJobPreservesBoundEmptyScratchDir(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	ts := &ToolService{}

	settledCh := make(chan JobSettledNotice, 1)
	ts.emitSettledFn = func(n JobSettledNotice) {
		select {
		case settledCh <- n:
		default:
		}
	}

	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "bg-scratch-keep",
		ToolUseID:      "toolu_scratch_1",
		ToolName:       "bash",
		Workdir:        "",
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input: map[string]any{
			"command":           "echo no-files-created",
			"run_in_background": true,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("run_in_background in temp chat failed: err=%v res=%#v", err, res)
	}
	if res.AssignedScratch == "" {
		t.Fatalf("expected AssignedScratch to be bound for background job")
	}

	select {
	case <-settledCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for background job to settle")
	}

	if _, statErr := os.Stat(res.AssignedScratch); statErr != nil {
		t.Fatalf("bound scratch dir %q should NOT be removed when background job finishes: %v", res.AssignedScratch, statErr)
	}
}

func TestRingBufferUTF8SafeStreaming(t *testing.T) {
	rb := newRingBuffer(1024)
	full := []byte("你好世界")
	// Write first 4 bytes: "你" (3 bytes) + first 1 byte of "好".
	_, _ = rb.Write(full[:4])
	chunk1, next1 := rb.readUTF8SafeFrom(0, false)
	if chunk1 != "你" || next1 != 3 {
		t.Fatalf("expected chunk1=%q next1=3, got chunk1=%q next1=%d", "你", chunk1, next1)
	}
	// Write remaining bytes and read again.
	_, _ = rb.Write(full[4:])
	chunk2, next2 := rb.readUTF8SafeFrom(next1, false)
	if chunk2 != "好世界" || next2 != int64(len(full)) {
		t.Fatalf("expected chunk2=%q next2=%d, got chunk2=%q next2=%d", "好世界", len(full), chunk2, next2)
	}
}

func TestPruneFinishedJobsPerSession(t *testing.T) {
	ts := &ToolService{}
	base := time.Now()
	for i := 0; i < maxFinishedJobsPerSession+5; i++ {
		j := &backgroundJob{
			sessionID:      "prune-sess",
			status:         jobStatusCompleted,
			exposedToModel: true,
			endedAt:        base.Add(time.Duration(i) * time.Second),
		}
		ts.registerBackgroundJob(j)
	}
	ts.pruneFinishedJobs("prune-sess")
	got := ts.ListBackgroundJobs("prune-sess")
	if len(got) != maxFinishedJobsPerSession {
		t.Fatalf("expected %d jobs after prune, got %d", maxFinishedJobsPerSession, len(got))
	}
}

func TestKillSessionBackgroundJobs(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	resA, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "sess-delete-a",
		ToolUseID:      "toolu_del_a",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input: map[string]any{
			"command":           "sleep 30",
			"run_in_background": true,
		},
	})
	if err != nil || resA.IsError {
		t.Fatalf("start job A failed: err=%v res=%#v", err, resA)
	}

	resB, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "sess-keep-b",
		ToolUseID:      "toolu_keep_b",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input: map[string]any{
			"command":           "sleep 30",
			"run_in_background": true,
		},
	})
	if err != nil || resB.IsError {
		t.Fatalf("start job B failed: err=%v res=%#v", err, resB)
	}
	t.Cleanup(func() {
		ts.KillAllBackgroundJobs()
	})

	start := time.Now()
	if err := ts.KillSessionBackgroundJobs("sess-delete-a"); err != nil {
		t.Fatalf("KillSessionBackgroundJobs failed: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("KillSessionBackgroundJobs took too long: %v", time.Since(start))
	}
	if gotA := ts.ListBackgroundJobs("sess-delete-a"); len(gotA) != 0 {
		t.Fatalf("expected 0 jobs left for deleted session A, got %#v", gotA)
	}
	if gotB := ts.ListBackgroundJobs("sess-keep-b"); len(gotB) != 1 || gotB[0].Status != jobStatusRunning {
		t.Fatalf("expected session B job to remain running, got %#v", gotB)
	}
}
