package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

const (
	PermissionReadOnly       = "read-only"
	PermissionWorkspaceWrite = "workspace-write"
	PermissionFullAccess     = "full-access"

	DecisionAllow = "allow"
	DecisionAsk   = "ask"
	DecisionDeny  = "deny"
)

// ToolService exposes Go-native file and shell tools to the frontend via mygo.Bind.
type ToolService struct {
	mu            sync.Mutex
	nextID        uint64
	cancels       map[string]map[uint64]context.CancelFunc
	nextJobID     uint64
	jobs          map[string]*backgroundJob
	sessionAllows map[string]map[string]bool
}

var globalToolService = &ToolService{
	cancels:       make(map[string]map[uint64]context.CancelFunc),
	jobs:          make(map[string]*backgroundJob),
	sessionAllows: make(map[string]map[string]bool),
}

// ToolClassifyResult represents the pre-execution policy decision for a tool call.
type ToolClassifyResult struct {
	Decision        string `json:"decision"` // "allow" | "ask" | "deny"
	Reason          string `json:"reason"`
	SessionAllowKey string `json:"sessionAllowKey,omitempty"` // Non-empty when UI may offer "Allow for this session"
}

// ToolFileDiff carries structured before/after text for UI diff rendering.
type ToolFileDiff struct {
	Path    string `json:"path"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// ToolExecuteRequest is the unified RPC payload for classifying and executing a client tool.
type ToolExecuteRequest struct {
	SessionID         string         `json:"sessionId"`
	ToolName          string         `json:"toolName"` // read | write | edit | delete_file | glob | grep | bash | job_output | job_kill | skill
	Input             map[string]any `json:"input"`
	Workdir           string         `json:"workdir"`                     // User workspace or existing scratchDir; empty triggers lazy scratch on write/edit/bash
	PermissionMode    string         `json:"permissionMode"`              // read-only | workspace-write | full-access
	Approved          bool           `json:"approved"`                    // True only after user approval; backend still re-verifies
	AllowSession      bool           `json:"allowSession,omitempty"`      // When true with Approved, remembers non-high-risk command prefix for this session
	ApprovalBlacklist []string       `json:"approvalBlacklist,omitempty"` // User-configured command blacklist for sandboxed workspace-write mode (nil uses defaults)
	SkillConfig       *SkillConfig   `json:"skillConfig,omitempty"`       // User-configured skill directories, priority order, and disabled skills
}

// ToolExecuteResult separates concise model output from rich UI metadata.
type ToolExecuteResult struct {
	ModelContent    string         `json:"modelContent"`
	IsError         bool           `json:"isError"`
	AssignedScratch string         `json:"assignedScratch,omitempty"`
	UIMeta          map[string]any `json:"uiMeta,omitempty"`
}

func normalizePermissionMode(mode string) string {
	switch mode {
	case PermissionReadOnly, PermissionWorkspaceWrite, PermissionFullAccess:
		return mode
	default:
		return PermissionWorkspaceWrite
	}
}

func allowOutsideForRequest(req ToolExecuteRequest) bool {
	perm := normalizePermissionMode(req.PermissionMode)
	return perm == PermissionFullAccess || (perm == PermissionWorkspaceWrite && req.Approved)
}

func isWriteFileTool(name string) bool {
	switch name {
	case "write", "edit", "delete_file":
		return true
	default:
		return false
	}
}

func isReadFileTool(name string) bool {
	switch name {
	case "read", "glob", "grep":
		return true
	default:
		return false
	}
}

func stringInput(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func intInput(m map[string]any, key string, fallback int) int {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return fallback
	}
}

func boolInput(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// ClassifyToolCall performs pre-execution policy enforcement for file and shell tools.
//   - read-only: allows read/glob/grep inside workspace & strict read-only bash allowlist; denies everything else
//   - workspace-write: allows workspace file ops & strict read-only bash; upgrades out-of-workspace file ops and non-read-only bash to ask
//   - full-access: allows cross-directory ops & standard commands; keeps ask for high-risk deletions/commands
func (s *ToolService) ClassifyToolCall(req ToolExecuteRequest) ToolClassifyResult {
	perm := normalizePermissionMode(req.PermissionMode)

	if req.ToolName == "bash" {
		res := classifyBashToolCall(req, perm)
		if perm == PermissionWorkspaceWrite && res.Decision == DecisionAsk && res.SessionAllowKey != "" {
			if s.isSessionAllowed(req.SessionID, res.SessionAllowKey) {
				return ToolClassifyResult{Decision: DecisionAllow}
			}
		}
		return res
	}
	if req.ToolName == "job_output" || req.ToolName == "job_kill" || req.ToolName == "skill" {
		return ToolClassifyResult{Decision: DecisionAllow}
	}
	if strings.HasPrefix(req.ToolName, "mcp__") {
		res := globalMCPService.classifyMCPToolCall(req, perm)
		if perm == PermissionWorkspaceWrite && res.Decision == DecisionAsk && res.SessionAllowKey != "" {
			if s.isSessionAllowed(req.SessionID, res.SessionAllowKey) {
				return ToolClassifyResult{Decision: DecisionAllow}
			}
		}
		return res
	}
	if isReadFileTool(req.ToolName) || isWriteFileTool(req.ToolName) {
		return classifyFileToolCall(req, perm)
	}
	return ToolClassifyResult{
		Decision: DecisionDeny,
		Reason:   fmt.Sprintf("unsupported tool: %s", req.ToolName),
	}
}

func extractTargetPath(req ToolExecuteRequest) string {
	switch req.ToolName {
	case "read", "write", "edit", "delete_file":
		return stringInput(req.Input, "file_path")
	case "glob", "grep":
		return stringInput(req.Input, "path")
	default:
		return ""
	}
}

// ExecuteTool re-verifies policy on the backend (fail-closed) and dispatches the requested tool.
func (s *ToolService) ExecuteTool(req ToolExecuteRequest) (ToolExecuteResult, error) {
	classification := s.ClassifyToolCall(req)
	switch classification.Decision {
	case DecisionDeny:
		return ToolExecuteResult{
			ModelContent: fmt.Sprintf("Permission denied: %s", classification.Reason),
			IsError:      true,
		}, fmt.Errorf("permission denied: %s", classification.Reason)
	case DecisionAsk:
		if !req.Approved {
			return ToolExecuteResult{
				ModelContent: fmt.Sprintf("Approval required: %s", classification.Reason),
				IsError:      true,
			}, fmt.Errorf("approval required: %s", classification.Reason)
		}
		if req.AllowSession && classification.SessionAllowKey != "" && normalizePermissionMode(req.PermissionMode) == PermissionWorkspaceWrite {
			s.grantSessionAllow(req.SessionID, classification.SessionAllowKey)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	callID := s.registerCancel(req.SessionID, cancel)
	defer s.unregisterCancel(req.SessionID, callID, cancel)

	switch req.ToolName {
	case "read":
		return executeRead(ctx, req)
	case "write":
		return executeWrite(ctx, req)
	case "edit":
		return executeEdit(ctx, req)
	case "delete_file":
		return executeDeleteFile(ctx, req)
	case "glob":
		return executeGlob(ctx, req)
	case "grep":
		return executeGrep(ctx, req)
	case "bash":
		return s.executeBash(ctx, req)
	case "job_output":
		return s.executeJobOutput(ctx, req)
	case "job_kill":
		return s.executeJobKill(req)
	case "skill":
		return executeSkill(req)
	default:
		if strings.HasPrefix(req.ToolName, "mcp__") {
			return globalMCPService.executeMCPTool(ctx, req)
		}
		return ToolExecuteResult{
			ModelContent: fmt.Sprintf("Unsupported tool: %s", req.ToolName),
			IsError:      true,
		}, fmt.Errorf("unsupported tool: %s", req.ToolName)
	}
}

// CancelSessionTools cancels all active tool executions (including running bash process groups) for sessionID.
func (s *ToolService) CancelSessionTools(sessionID string) error {
	s.mu.Lock()
	active := s.cancels[sessionID]
	delete(s.cancels, sessionID)
	s.mu.Unlock()

	for _, cancel := range active {
		cancel()
	}
	return nil
}

// GetDefaultApprovalBlacklist returns the built-in command blacklist used in sandboxed workspace-write mode.
func (s *ToolService) GetDefaultApprovalBlacklist() []string {
	out := make([]string, len(defaultSandboxApprovalBlacklist))
	copy(out, defaultSandboxApprovalBlacklist)
	return out
}

// ClearSessionApprovals revokes all in-memory session-scoped command approvals for sessionID.
func (s *ToolService) ClearSessionApprovals(sessionID string) error {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	delete(s.sessionAllows, sessionID)
	s.mu.Unlock()
	return nil
}

// ReleaseSessionSandbox revokes and removes any pooled sandbox private temp directories
// associated with sessionID once active commands finish.
func (s *ToolService) ReleaseSessionSandbox(sessionID string) error {
	releaseSessionSandboxTemp(sessionID)
	return nil
}

// CleanupWorkspaceSandboxACL revokes the standing Windows ACL capability ACE and
// Low mandatory integrity label previously materialized on workspacePath.
// Returns (false, nil) on non-Windows platforms.
func (s *ToolService) CleanupWorkspaceSandboxACL(workspacePath string) (bool, error) {
	return cleanupWorkspaceSandboxACL(workspacePath)
}

func (s *ToolService) isSessionAllowed(sessionID, key string) bool {
	if sessionID == "" || key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionAllows[sessionID][key]
}

func (s *ToolService) grantSessionAllow(sessionID, key string) {
	if sessionID == "" || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionAllows == nil {
		s.sessionAllows = make(map[string]map[string]bool)
	}
	if s.sessionAllows[sessionID] == nil {
		s.sessionAllows[sessionID] = make(map[string]bool)
	}
	s.sessionAllows[sessionID][key] = true
}

func (s *ToolService) registerCancel(sessionID string, cancel context.CancelFunc) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancels == nil {
		s.cancels = make(map[string]map[uint64]context.CancelFunc)
	}
	s.nextID++
	id := s.nextID
	if sessionID != "" {
		if s.cancels[sessionID] == nil {
			s.cancels[sessionID] = make(map[uint64]context.CancelFunc)
		}
		s.cancels[sessionID][id] = cancel
	}
	return id
}

func (s *ToolService) unregisterCancel(sessionID string, callID uint64, cancel context.CancelFunc) {
	cancel()
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.cancels[sessionID]; m != nil {
		delete(m, callID)
		if len(m) == 0 {
			delete(s.cancels, sessionID)
		}
	}
}
