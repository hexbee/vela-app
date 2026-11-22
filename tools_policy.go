package main

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// dangerousShellTokens rejects compound commands, pipes, redirections, and command substitutions
// from the strict read-only allowlist.
var dangerousShellTokens = []string{
	"|", ">", "<", ";", "&", "\n", "\r", "$(", "`",
}

// powerShellWriteCmdlets are explicitly denied from the read-only allowlist on all platforms.
var powerShellWriteCmdlets = map[string]bool{
	"out-file":          true,
	"set-content":       true,
	"add-content":       true,
	"clear-content":     true,
	"remove-item":       true,
	"move-item":         true,
	"copy-item":         true,
	"new-item":          true,
	"rename-item":       true,
	"invoke-expression": true,
	"iex":               true,
	"start-process":     true,
	"stop-process":      true,
	"kill":              true,
	"rm":                true,
	"del":               true,
	"rmdir":             true,
	"mv":                true,
	"cp":                true,
}

// highRiskCommands require explicit one-time confirmation even in full-access mode.
var highRiskCommands = map[string]bool{
	"rm":                true,
	"rmdir":             true,
	"sudo":              true,
	"su":                true,
	"mkfs":              true,
	"dd":                true,
	"chmod":             true,
	"chown":             true,
	"shutdown":          true,
	"reboot":            true,
	"halt":              true,
	"kill":              true,
	"killall":           true,
	"pkill":             true,
	"diskutil":          true,
	"format":            true,
	"remove-item":       true,
	"ri":                true,
	"del":               true,
	"erase":             true,
	"rd":                true,
	"stop-process":      true,
	"spps":              true,
	"taskkill":          true,
	"invoke-expression": true,
	"iex":               true,
	"icacls":            true,
	"takeown":           true,
	"reg":               true,
	"sc":                true,
	"diskpart":          true,
	"bcdedit":           true,
}

func classifyFileToolCall(req ToolExecuteRequest, perm string) ToolClassifyResult {
	if perm == PermissionReadOnly && isWriteFileTool(req.ToolName) {
		return ToolClassifyResult{
			Decision: DecisionDeny,
			Reason:   fmt.Sprintf("tool %q is not allowed in read-only mode", req.ToolName),
		}
	}

	root, _, err := resolveRootForRequest(req, false)
	if err != nil {
		return ToolClassifyResult{
			Decision: DecisionDeny,
			Reason:   err.Error(),
		}
	}

	rawPath := extractTargetPath(req)
	if rawPath == "" {
		if req.ToolName == "glob" || req.ToolName == "grep" {
			if root == "" {
				return ToolClassifyResult{
					Decision: DecisionDeny,
					Reason:   fmt.Sprintf("tool %q requires an active workspace or explicit path", req.ToolName),
				}
			}
			return ToolClassifyResult{Decision: DecisionAllow}
		}
		return ToolClassifyResult{Decision: DecisionAllow}
	}

	followLeaf := req.ToolName != "delete_file"
	resolved, withinRoot, err := resolveAndValidatePath(rawPath, root, true, followLeaf)
	if err != nil {
		if errors.Is(err, errBrokenSymlink) {
			return ToolClassifyResult{
				Decision: DecisionDeny,
				Reason:   err.Error(),
			}
		}
		if root == "" && perm != PermissionFullAccess {
			return ToolClassifyResult{
				Decision: DecisionDeny,
				Reason:   err.Error(),
			}
		}
		return ToolClassifyResult{Decision: DecisionAllow}
	}

	// Never allow deleting filesystem root or workspace root.
	if req.ToolName == "delete_file" {
		if resolved == filepath.Dir(resolved) {
			return ToolClassifyResult{
				Decision: DecisionDeny,
				Reason:   "refusing to delete filesystem root",
			}
		}
		if root != "" {
			if canonicalRoot, cErr := canonicalizePath(root); cErr == nil && resolved == canonicalRoot {
				return ToolClassifyResult{
					Decision: DecisionDeny,
					Reason:   "refusing to delete workspace root directory",
				}
			}
		}
	}

	if !withinRoot {
		switch perm {
		case PermissionReadOnly:
			return ToolClassifyResult{
				Decision: DecisionDeny,
				Reason:   fmt.Sprintf("path %q is outside the allowed workspace", rawPath),
			}
		case PermissionWorkspaceWrite:
			return ToolClassifyResult{
				Decision: DecisionAsk,
				Reason:   fmt.Sprintf("path %q is outside the current workspace and requires one-time approval", rawPath),
			}
		case PermissionFullAccess:
			if req.ToolName == "delete_file" {
				return ToolClassifyResult{
					Decision: DecisionAsk,
					Reason:   fmt.Sprintf("deleting %q outside the workspace requires one-time confirmation", rawPath),
				}
			}
			return ToolClassifyResult{Decision: DecisionAllow}
		}
	}

	return ToolClassifyResult{Decision: DecisionAllow}
}

func permissionRank(mode string) (int, bool) {
	switch mode {
	case PermissionReadOnly:
		return 0, true
	case PermissionWorkspaceWrite:
		return 1, true
	case PermissionFullAccess:
		return 2, true
	default:
		return -1, false
	}
}

// parseSandboxEscalation validates optional per-call sandbox_permissions + justification
// escalation fields against basePerm (strictly wider ladder, fail-closed).
func parseSandboxEscalation(req ToolExecuteRequest, basePerm string) (effectivePerm string, escalated bool, justification string, errMsg string) {
	rawTarget := strings.TrimSpace(stringInput(req.Input, "sandbox_permissions"))
	justification = strings.TrimSpace(stringInput(req.Input, "justification"))

	if rawTarget == "" {
		if justification != "" {
			return basePerm, false, "", "justification requires sandbox_permissions"
		}
		return basePerm, false, "", ""
	}

	targetRank, ok := permissionRank(rawTarget)
	if !ok {
		return basePerm, false, "", fmt.Sprintf("invalid sandbox_permissions %q (expected %q or %q)", rawTarget, PermissionWorkspaceWrite, PermissionFullAccess)
	}
	baseRank, _ := permissionRank(basePerm)
	if targetRank == baseRank {
		// Repeating the current effective mode is a no-op and needs no escalation.
		return basePerm, false, "", ""
	}
	if targetRank < baseRank {
		return basePerm, false, "", fmt.Sprintf("sandbox_permissions %q is narrower than current mode %q", rawTarget, basePerm)
	}
	if justification == "" {
		return basePerm, false, "", fmt.Sprintf("sandbox_permissions escalation (%s -> %s) requires a non-empty justification", basePerm, rawTarget)
	}
	return rawTarget, true, justification, ""
}

func sandboxEscalationHint(mode string) string {
	switch mode {
	case PermissionReadOnly:
		return `[sandbox: escalation available — retry this exact command once with sandbox_permissions ("workspace-write" or "full-access", the narrowest mode that suffices) + justification to request one-time user approval]`
	case PermissionWorkspaceWrite:
		return `[sandbox: escalation available — retry this exact command once with sandbox_permissions ("full-access") + justification to request one-time user approval]`
	default:
		return ""
	}
}

func classifyBashToolCall(req ToolExecuteRequest, perm string) ToolClassifyResult {
	cmdStr := strings.TrimSpace(stringInput(req.Input, "command"))
	if cmdStr == "" {
		// Let ExecuteTool return a clean parameter error.
		return ToolClassifyResult{Decision: DecisionAllow}
	}

	effectivePerm, escalated, justification, errMsg := parseSandboxEscalation(req, perm)
	if errMsg != "" {
		return ToolClassifyResult{
			Decision: DecisionDeny,
			Reason:   errMsg,
		}
	}

	// Validate explicit workdir override if supplied.
	inputWorkdir := strings.TrimSpace(stringInput(req.Input, "workdir"))
	if inputWorkdir != "" && strings.TrimSpace(req.Workdir) != "" {
		_, withinRoot, err := resolveAndValidatePath(inputWorkdir, req.Workdir, true, true)
		if err != nil {
			if errors.Is(err, errBrokenSymlink) {
				return ToolClassifyResult{Decision: DecisionDeny, Reason: err.Error()}
			}
		} else if !withinRoot {
			if effectivePerm == PermissionReadOnly {
				return ToolClassifyResult{
					Decision: DecisionDeny,
					Reason:   fmt.Sprintf("workdir %q is outside the allowed workspace", inputWorkdir),
				}
			}
			if effectivePerm == PermissionWorkspaceWrite && !escalated {
				return ToolClassifyResult{
					Decision: DecisionAsk,
					Reason:   fmt.Sprintf("running command in %q outside the workspace requires one-time approval", inputWorkdir),
				}
			}
		}
	} else if inputWorkdir != "" && strings.TrimSpace(req.Workdir) == "" && effectivePerm == PermissionReadOnly {
		return ToolClassifyResult{
			Decision: DecisionDeny,
			Reason:   "explicit workdir is not allowed in read-only mode without an active workspace",
		}
	}

	// Any strictly wider sandbox_permissions request ALWAYS requires one-time user approval
	// (empty SessionAllowKey) with the model's justification surfaced in the approval prompt.
	if escalated {
		return ToolClassifyResult{
			Decision: DecisionAsk,
			Reason:   fmt.Sprintf("one-time sandbox escalation (%s -> %s): %s", perm, effectivePerm, justification),
		}
	}

	if isStrictReadOnlyCommand(cmdStr) {
		return ToolClassifyResult{Decision: DecisionAllow}
	}

	switch perm {
	case PermissionReadOnly:
		return ToolClassifyResult{
			Decision: DecisionDeny,
			Reason:   "command is not in the strict read-only allowlist",
		}
	case PermissionWorkspaceWrite:
		// When OS sandbox (macOS sandbox-exec / Linux bwrap) is active, default-allow workspace commands
		// and only prompt for commands matching the configurable approval blacklist.
		if osSandboxAvailableFn() {
			rules := req.ApprovalBlacklist
			if rules == nil {
				rules = defaultSandboxApprovalBlacklist
			}
			if matched, hit := matchApprovalBlacklist(cmdStr, rules); hit {
				return ToolClassifyResult{
					Decision: DecisionAsk,
					Reason:   fmt.Sprintf("command matches approval blacklist (%s)", matched),
				}
			}
			return ToolClassifyResult{Decision: DecisionAllow}
		}
		if isHighRiskCommand(cmdStr) || isExternalSideEffectCommand(cmdStr) {
			return ToolClassifyResult{
				Decision: DecisionAsk,
				Reason:   "destructive or network command requires one-time approval",
			}
		}
		return ToolClassifyResult{
			Decision:        DecisionAsk,
			Reason:          "non-read-only command requires approval",
			SessionAllowKey: deriveSessionAllowKey(cmdStr),
		}
	case PermissionFullAccess:
		if isHighRiskCommand(cmdStr) {
			return ToolClassifyResult{
				Decision: DecisionAsk,
				Reason:   "high-risk command requires one-time confirmation",
			}
		}
		return ToolClassifyResult{Decision: DecisionAllow}
	default:
		return ToolClassifyResult{
			Decision: DecisionAsk,
			Reason:   "command requires one-time approval",
		}
	}
}

var commonSandboxApprovalBlacklist = []string{
	"rm",
	"rmdir",
	"shutdown",
	"kill",
	"format",
	"git reset",
	"git clean",
	"git checkout",
	"git restore",
	"git rebase",
	"git merge",
	"git push",
	"curl",
	"wget",
	"ssh",
	"scp",
	"rsync",
	"nc",
	"netcat",
	"telnet",
	"ftp",
	"sftp",
	"gh",
	"aws",
	"gcloud",
	"az",
	"twine",
	"npm publish",
	"pnpm publish",
	"yarn publish",
	"cargo publish",
}

var unixSandboxApprovalBlacklist = []string{
	"sudo",
	"su",
	"mkfs",
	"dd",
	"chmod",
	"chown",
	"reboot",
	"halt",
	"killall",
	"pkill",
}

var windowsSandboxApprovalBlacklist = []string{
	"remove-item",
	"ri",
	"del",
	"erase",
	"rd",
	"stop-process",
	"spps",
	"taskkill",
	"start-process",
	"saps",
	"invoke-expression",
	"iex",
	"icacls",
	"takeown",
	"reg",
	"sc",
	"net",
	"netsh",
	"diskpart",
	"bcdedit",
	"invoke-webrequest",
	"iwr",
	"invoke-restmethod",
	"irm",
	"bitsadmin",
	"certutil",
}

// defaultSandboxApprovalBlacklistForOS returns the built-in sandbox approval blacklist
// tailored to the target OS (common cross-platform rules + OS-specific rules).
func defaultSandboxApprovalBlacklistForOS(goos string) []string {
	out := make([]string, 0, len(commonSandboxApprovalBlacklist)+len(windowsSandboxApprovalBlacklist))
	out = append(out, commonSandboxApprovalBlacklist...)
	switch goos {
	case "windows":
		out = append(out, windowsSandboxApprovalBlacklist...)
	case "darwin":
		out = append(out, unixSandboxApprovalBlacklist...)
		out = append(out, "diskutil")
	default:
		out = append(out, unixSandboxApprovalBlacklist...)
	}
	return out
}

// defaultSandboxApprovalBlacklist defines commands that require explicit one-time approval
// even when OS-level sandbox-exec / bwrap / Windows ACL sandbox is active in workspace-write mode.
var defaultSandboxApprovalBlacklist = defaultSandboxApprovalBlacklistForOS(runtime.GOOS)

// extractAllCommandSegments splits a shell command across &&, ||, ;, |, &, newlines, subshells (...),
// and recursively extracts inner $(...) / `...` substitutions so blacklisted commands cannot hide inside.
func extractAllCommandSegments(s string) []string {
	var segs []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	flush := func() {
		seg := strings.TrimSpace(cur.String())
		cur.Reset()
		if seg != "" {
			segs = append(segs, seg)
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			cur.WriteByte(c)
			continue
		}
		// Extract backtick command substitutions even inside double quotes.
		if c == '`' {
			end := i + 1
			for end < len(s) && s[end] != '`' {
				if s[end] == '\\' && end+1 < len(s) {
					end++
				}
				end++
			}
			if end < len(s) {
				inner := s[i+1 : end]
				segs = append(segs, extractAllCommandSegments(inner)...)
				i = end
				continue
			}
		}
		// Extract $(...) command substitutions even inside double quotes.
		if c == '$' && i+1 < len(s) && s[i+1] == '(' {
			depth := 1
			end := i + 2
			for end < len(s) && depth > 0 {
				switch s[end] {
				case '(':
					depth++
				case ')':
					depth--
				}
				end++
			}
			if depth == 0 {
				inner := s[i+2 : end-1]
				segs = append(segs, extractAllCommandSegments(inner)...)
				i = end - 1
				continue
			}
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			escaped = true
			cur.WriteByte(c)
		case '\'':
			inSingle = true
			cur.WriteByte(c)
		case '"':
			inDouble = true
			cur.WriteByte(c)
		case ';', '|', '&', '\n', '\r', '(', ')':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segs
}

// matchApprovalBlacklist checks whether any sub-command in cmdStr matches one of the configured rules.
// Rules can be a single command name ("rm", "curl") or a command + subcommand prefix ("git push", "npm publish").
func matchApprovalBlacklist(cmdStr string, rules []string) (string, bool) {
	if len(rules) == 0 {
		return "", false
	}
	type parsedRule struct {
		raw  string
		cmd  string
		sub  string
		more bool
	}
	parsed := make([]parsedRule, 0, len(rules))
	for _, r := range rules {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(strings.ToLower(trimmed))
		if len(fields) == 0 {
			continue
		}
		pr := parsedRule{raw: trimmed, cmd: fields[0]}
		if len(fields) >= 2 {
			pr.sub = fields[1]
			pr.more = len(fields) > 2
		}
		parsed = append(parsed, pr)
	}
	if len(parsed) == 0 {
		return "", false
	}

	segments := extractAllCommandSegments(cmdStr)
	for _, seg := range segments {
		words, ok := splitShellWords(seg)
		if !ok || len(words) == 0 {
			// Fallback to whitespace split if a segment had partial quotes.
			words = strings.Fields(seg)
		}
		// Skip leading inline env assignments (FOO=bar cmd ...) and wrapper keywords.
		for len(words) > 0 && (strings.Contains(words[0], "=") || words[0] == "!" || words[0] == "time" || words[0] == "nohup" || words[0] == "exec" || words[0] == "command") {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		cmd := extractCommandNameFromSegment(seg, words[0])
		var sub string
		for _, w := range words[1:] {
			if !strings.HasPrefix(w, "-") {
				sub = strings.ToLower(w)
				break
			}
		}
		segLower := strings.ToLower(seg)

		for _, pr := range parsed {
			if pr.more {
				if strings.Contains(segLower, strings.ToLower(pr.raw)) {
					return pr.raw, true
				}
				continue
			}
			if pr.sub == "" {
				if cmd == pr.cmd {
					// Exempt curl/wget calls that exclusively target local loopback addresses (localhost / 127.0.0.1 / [::1]).
					if (cmd == "curl" || cmd == "wget") && isLoopbackOnlyHTTPCommand(cmd, words[1:]) {
						continue
					}
					return pr.raw, true
				}
			} else {
				if cmd == pr.cmd && (sub == pr.sub || (len(words) >= 2 && strings.ToLower(words[1]) == pr.sub)) {
					return pr.raw, true
				}
			}
		}
	}
	return "", false
}

// isLoopbackOnlyHTTPCommand returns true only when a curl/wget invocation contains at least one
// explicit URL argument, uses no indirect/proxy/config flags, and every URL targets localhost, 127.0.0.1, or ::1.
func isLoopbackOnlyHTTPCommand(cmd string, args []string) bool {
	var valueFlags map[string]bool
	var forbiddenFlags map[string]bool
	if cmd == "curl" {
		valueFlags = map[string]bool{
			"-o": true, "--output": true, "-w": true, "--write-out": true,
			"-H": true, "--header": true, "-X": true, "--request": true,
			"-d": true, "--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true,
			"-F": true, "--form": true, "-u": true, "--user": true,
			"-A": true, "--user-agent": true, "-e": true, "--referer": true,
			"-b": true, "--cookie": true, "-c": true, "--cookie-jar": true,
			"-m": true, "--max-time": true, "--connect-timeout": true,
			"-T": true, "--upload-file": true, "--url": true,
		}
		forbiddenFlags = map[string]bool{
			"-K": true, "--config": true, "-x": true, "--proxy": true,
			"--socks4": true, "--socks5": true, "--socks5-hostname": true,
			"--resolve": true, "--connect-to": true, "--next": true, "-:": true,
		}
	} else {
		valueFlags = map[string]bool{
			"-O": true, "--output-document": true, "--header": true, "--method": true,
			"--body-data": true, "-T": true, "--timeout": true, "-t": true, "--tries": true,
			"-U": true, "--user-agent": true, "-P": true, "--directory-prefix": true,
		}
		forbiddenFlags = map[string]bool{
			"-i": true, "--input-file": true, "--config": true,
		}
	}

	urlCount := 0
	endOfFlags := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !endOfFlags && arg == "--" {
			endOfFlags = true
			continue
		}
		if !endOfFlags && strings.HasPrefix(arg, "-") && arg != "-" {
			flagName := arg
			hasEq := false
			eqVal := ""
			if eqIdx := strings.IndexByte(arg, '='); eqIdx > 0 {
				flagName = arg[:eqIdx]
				hasEq = true
				eqVal = arg[eqIdx+1:]
			}
			if forbiddenFlags[flagName] {
				return false
			}
			if flagName == "--url" {
				target := eqVal
				if !hasEq {
					if i+1 >= len(args) {
						return false
					}
					i++
					target = args[i]
				}
				if !isLoopbackURL(target) {
					return false
				}
				urlCount++
				continue
			}
			if valueFlags[flagName] {
				if !hasEq {
					if i+1 >= len(args) {
						return false
					}
					i++
				}
				continue
			}
			if !strings.HasPrefix(arg, "--") && len(arg) > 2 {
				for idx := 1; idx < len(arg); idx++ {
					short := "-" + string(arg[idx])
					if forbiddenFlags[short] {
						return false
					}
					if valueFlags[short] {
						if idx == len(arg)-1 {
							if i+1 >= len(args) {
								return false
							}
							i++
						}
						break
					}
				}
			}
			continue
		}

		if !isLoopbackURL(arg) {
			return false
		}
		urlCount++
	}
	return urlCount > 0
}

func isLoopbackURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	if strings.ContainsAny(trimmed, "{}[]") && !strings.Contains(trimmed, "[::1]") {
		return false
	}
	candidate := trimmed
	if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	u, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

var externalSideEffectCommands = map[string]bool{
	"curl":              true,
	"wget":              true,
	"invoke-webrequest": true,
	"iwr":               true,
	"invoke-restmethod": true,
	"irm":               true,
	"bitsadmin":         true,
	"certutil":          true,
	"ssh":               true,
	"scp":               true,
	"rsync":             true,
	"nc":                true,
	"netcat":            true,
	"telnet":            true,
	"ftp":               true,
	"sftp":              true,
	"gh":                true,
	"aws":               true,
	"gcloud":            true,
	"az":                true,
	"twine":             true,
}

// normalizeCommandName extracts the lowercase command basename across both POSIX
// and Windows path separators and strips common Windows executable extensions.
func normalizeCommandName(raw string) string {
	s := strings.TrimSpace(raw)
	if idx := strings.LastIndexAny(s, `/\`); idx >= 0 {
		s = s[idx+1:]
	}
	s = strings.ToLower(s)
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
		if strings.HasSuffix(s, ext) && len(s) > len(ext) {
			s = strings.TrimSuffix(s, ext)
			break
		}
	}
	return s
}

// extractCommandNameFromSegment preserves Windows backslash paths (such as
// C:\Windows\System32\icacls.exe or "C:\Program Files\...\rm.exe") that POSIX
// backslash escaping in splitShellWords would otherwise flatten.
func extractCommandNameFromSegment(seg, fallbackWord string) string {
	s := strings.TrimSpace(seg)
	for s != "" {
		var tok string
		if s[0] == '"' || s[0] == '\'' {
			q := s[0]
			if end := strings.IndexByte(s[1:], q); end >= 0 {
				tok = s[1 : 1+end]
				s = strings.TrimSpace(s[2+end:])
			} else {
				break
			}
		} else {
			end := strings.IndexAny(s, " \t")
			if end < 0 {
				tok = s
				s = ""
			} else {
				tok = s[:end]
				s = strings.TrimSpace(s[end:])
			}
		}
		if strings.Contains(tok, "=") || tok == "!" || tok == "time" || tok == "nohup" || tok == "exec" || tok == "command" {
			continue
		}
		if strings.Contains(tok, `\`) {
			return normalizeCommandName(tok)
		}
		break
	}
	return normalizeCommandName(fallbackWord)
}

func isExternalSideEffectCommand(cmdStr string) bool {
	words, ok := splitShellWords(cmdStr)
	if !ok || len(words) == 0 {
		return true
	}
	cmd := extractCommandNameFromSegment(cmdStr, words[0])
	if externalSideEffectCommands[cmd] {
		return true
	}
	if cmd == "git" && len(words) >= 2 {
		switch strings.ToLower(words[1]) {
		case "push", "fetch", "pull", "clone", "reset", "clean", "checkout", "restore", "rebase", "merge":
			return true
		}
	}
	if (cmd == "npm" || cmd == "pnpm" || cmd == "yarn" || cmd == "cargo") && len(words) >= 2 {
		if strings.ToLower(words[1]) == "publish" {
			return true
		}
	}
	return false
}

// stripStderrToStdout removes standalone unquoted "2>&1" stream merges (which never write to disk).
func stripStderrToStdout(s string) string {
	var b strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(s); {
		c := s[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			i++
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			b.WriteByte(c)
			i++
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			b.WriteByte(c)
			i++
			continue
		}
		if c == '\\' {
			escaped = true
			b.WriteByte(c)
			i++
			continue
		}
		if c == '\'' {
			inSingle = true
			b.WriteByte(c)
			i++
			continue
		}
		if c == '"' {
			inDouble = true
			b.WriteByte(c)
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "2>&1") {
			prevOK := i == 0 || s[i-1] == ' ' || s[i-1] == '\t'
			end := i + 4
			nextOK := end == len(s) || s[end] == ' ' || s[end] == '\t' || s[end] == '|' || s[end] == ';' || s[end] == '&'
			if prevOK && nextOK {
				i = end
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return strings.TrimSpace(b.String())
}

// splitTopLevelCommandSegments splits a command on unquoted "&&" or ";".
// Returns (nil, false) on unterminated quotes, background "&", or "||".
func splitTopLevelCommandSegments(s string) ([]string, bool) {
	var segs []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	flush := func() bool {
		seg := strings.TrimSpace(cur.String())
		cur.Reset()
		if seg == "" {
			return false
		}
		segs = append(segs, seg)
		return true
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			cur.WriteByte(c)
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			escaped = true
			cur.WriteByte(c)
		case '\'':
			inSingle = true
			cur.WriteByte(c)
		case '"':
			inDouble = true
			cur.WriteByte(c)
		case ';':
			if !flush() {
				return nil, false
			}
		case '&':
			if i+1 < len(s) && s[i+1] == '&' {
				i++
				if !flush() {
					return nil, false
				}
			} else {
				return nil, false
			}
		case '|':
			if i+1 < len(s) && s[i+1] == '|' {
				return nil, false
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if inSingle || inDouble || escaped {
		return nil, false
	}
	if strings.TrimSpace(cur.String()) != "" {
		flush()
	}
	if len(segs) == 0 {
		return nil, false
	}
	return segs, true
}

// splitTopLevelPipes splits a single segment on unquoted "|".
func splitTopLevelPipes(s string) ([]string, bool) {
	var stages []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			cur.WriteByte(c)
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			escaped = true
			cur.WriteByte(c)
		case '\'':
			inSingle = true
			cur.WriteByte(c)
		case '"':
			inDouble = true
			cur.WriteByte(c)
		case '|':
			stage := strings.TrimSpace(cur.String())
			cur.Reset()
			if stage == "" {
				return nil, false
			}
			stages = append(stages, stage)
		default:
			cur.WriteByte(c)
		}
	}
	if inSingle || inDouble || escaped {
		return nil, false
	}
	last := strings.TrimSpace(cur.String())
	if last == "" {
		return nil, false
	}
	stages = append(stages, last)
	return stages, true
}

// isSandboxSafeWorkspaceCommand returns true for local build/test/lint/format commands,
// read-only pipelines, and "&&" / ";" chains where every segment is individually safe
// when OS-level sandbox-exec / bwrap restricts writes to the workspace.
func isSandboxSafeWorkspaceCommand(cmdStr string) bool {
	cleaned := stripStderrToStdout(cmdStr)
	if cleaned == "" {
		return false
	}
	// Reject file redirections, command substitution, and multi-line scripts.
	for _, tok := range []string{">", "<", "\n", "\r", "$(", "`"} {
		if strings.Contains(cleaned, tok) {
			return false
		}
	}

	segments, ok := splitTopLevelCommandSegments(cleaned)
	if !ok {
		return false
	}
	for _, seg := range segments {
		if !isSandboxSafeSegment(seg) {
			return false
		}
	}
	return true
}

func isSandboxSafeSegment(seg string) bool {
	stages, ok := splitTopLevelPipes(seg)
	if !ok || len(stages) == 0 {
		return false
	}
	if len(stages) == 1 {
		return isStrictReadOnlyCommand(stages[0]) || isSandboxSafeDevCommand(stages[0])
	}
	// Pipeline: first stage may be a safe dev command or read-only command; all downstream stages must be strict read-only.
	if !isStrictReadOnlyCommand(stages[0]) && !isSandboxSafeDevCommand(stages[0]) {
		return false
	}
	for _, stage := range stages[1:] {
		if !isStrictReadOnlyCommand(stage) {
			return false
		}
	}
	return true
}

func isSandboxSafeDevCommand(cmdStr string) bool {
	if isHighRiskCommand(cmdStr) || isExternalSideEffectCommand(cmdStr) {
		return false
	}
	words, ok := splitShellWords(cmdStr)
	if !ok || len(words) == 0 || strings.Contains(words[0], "=") {
		return false
	}
	cmd := strings.ToLower(filepath.Base(words[0]))
	args := words[1:]

	switch cmd {
	case "go":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "test", "build", "vet", "fmt", "generate":
			return true
		case "mod":
			return len(args) >= 2 && (args[1] == "tidy" || args[1] == "verify")
		default:
			return false
		}
	case "gofmt":
		return true
	case "tsc", "oxlint", "oxfmt", "eslint", "prettier", "vitest", "jest", "rspack", "vite", "pytest", "ruff", "mypy", "flake8", "black":
		return true
	case "mygo":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "generate", "vet", "doctor", "build", "check":
			return true
		case "native":
			return len(args) >= 2 && (args[1] == "check" || args[1] == "build")
		default:
			return false
		}
	case "pnpm", "npm", "yarn":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "test", "build", "lint", "type", "typecheck", "format", "check", "generate":
			return true
		case "run":
			return len(args) >= 2 && !strings.Contains(strings.ToLower(args[1]), "publish") && !strings.Contains(strings.ToLower(args[1]), "deploy")
		default:
			return false
		}
	case "cargo":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "test", "check", "build", "clippy", "fmt":
			return true
		default:
			return false
		}
	case "make", "cmake":
		return true
	default:
		return false
	}
}

// deriveSessionAllowKey computes a concise command prefix key (e.g. "python3", "pnpm run")
// for session-scoped approval. Returns "" for compound/unsafe commands so they remain one-time only.
func deriveSessionAllowKey(cmdStr string) string {
	for _, tok := range dangerousShellTokens {
		if strings.Contains(cmdStr, tok) {
			return ""
		}
	}
	words, ok := splitShellWords(cmdStr)
	if !ok || len(words) == 0 || strings.Contains(words[0], "=") {
		return ""
	}
	cmd := strings.ToLower(filepath.Base(words[0]))
	if powerShellWriteCmdlets[cmd] || highRiskCommands[cmd] || externalSideEffectCommands[cmd] {
		return ""
	}
	if len(words) >= 2 && !strings.HasPrefix(words[1], "-") {
		switch cmd {
		case "git", "go", "npm", "pnpm", "yarn", "cargo", "mygo":
			return cmd + " " + strings.ToLower(words[1])
		}
	}
	return cmd
}

func isHighRiskCommand(cmdStr string) bool {
	lower := strings.ToLower(cmdStr)
	for _, tok := range []string{"rm -rf", "rm -fr", "sudo ", "mkfs", "dd if=", "shutdown", "reboot", "remove-item"} {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	words, ok := splitShellWords(cmdStr)
	if !ok || len(words) == 0 {
		return true
	}
	cmd := extractCommandNameFromSegment(cmdStr, words[0])
	return highRiskCommands[cmd]
}

// isStrictReadOnlyCommand returns true ONLY for single, non-compound commands whose
// subcommand and every flag are explicitly allowlisted as side-effect-free read-only operations.
func isStrictReadOnlyCommand(cmdStr string) bool {
	cleaned := stripStderrToStdout(cmdStr)
	for _, tok := range dangerousShellTokens {
		if strings.Contains(cleaned, tok) {
			return false
		}
	}

	words, ok := splitShellWords(cleaned)
	if !ok || len(words) == 0 {
		return false
	}

	// Reject inline env assignments (e.g. FOO=bar cmd) from automatic allowlist.
	if strings.Contains(words[0], "=") {
		return false
	}

	cmd := extractCommandNameFromSegment(cleaned, words[0])
	if powerShellWriteCmdlets[cmd] {
		return false
	}
	args := words[1:]

	switch cmd {
	case "pwd":
		return allFlagsAllowed(args, map[string]bool{"-L": true, "-P": true}, nil)
	case "whoami", "hostname", "arch", "date", "id":
		return len(args) == 0
	case "uname":
		return allFlagsAllowed(args, map[string]bool{
			"-a": true, "-s": true, "-n": true, "-r": true, "-v": true, "-m": true, "-p": true,
		}, nil)
	case "which":
		return allFlagsAllowed(args, map[string]bool{"-a": true}, nil)
	case "echo":
		return allFlagsAllowed(args, map[string]bool{"-n": true}, nil)
	case "ls":
		return isAllowedLsArgs(args)
	case "cat":
		return allFlagsAllowed(args, map[string]bool{"-n": true, "-b": true, "-s": true}, nil)
	case "head":
		return isAllowedHeadTailArgs(args)
	case "tail":
		// Explicitly exclude -f / -F (follow) so read-only commands never block indefinitely.
		return isAllowedHeadTailArgs(args)
	case "wc":
		return allFlagsAllowed(args, map[string]bool{"-l": true, "-w": true, "-c": true, "-m": true}, nil)
	case "file":
		return allFlagsAllowed(args, map[string]bool{"-b": true, "-i": true, "--mime-type": true, "--brief": true}, nil)
	case "stat":
		return allFlagsAllowed(args, map[string]bool{"-x": true}, nil)
	case "realpath", "dirname", "basename":
		return allFlagsAllowed(args, nil, nil)
	case "grep":
		return allFlagsAllowed(args, map[string]bool{
			"-n": true, "-i": true, "-r": true, "-R": true, "-l": true, "-c": true,
			"-E": true, "-F": true, "-H": true, "-h": true, "-v": true, "-w": true,
			"-o": true, "-q": true, "-s": true, "--line-number": true, "--ignore-case": true,
			"--files-with-matches": true, "--count": true, "--recursive": true,
		}, map[string]bool{
			"-A": true, "-B": true, "-C": true, "-m": true, "-e": true, "--include": true, "--exclude": true,
		})
	case "rg":
		return allFlagsAllowed(args, map[string]bool{
			"-n": true, "-i": true, "-l": true, "-c": true, "-F": true, "-w": true,
			"-S": true, "--smart-case": true, "--line-number": true, "--ignore-case": true,
			"--files": true, "--files-with-matches": true, "--count": true, "--hidden": true,
		}, map[string]bool{
			"-A": true, "-B": true, "-C": true, "-g": true, "--glob": true, "-t": true, "--type": true, "-m": true,
		})
	case "find":
		return isAllowedFindArgs(args)
	case "git":
		return isAllowedGitArgs(args)
	case "go":
		return isAllowedGoArgs(args)
	case "node", "npm", "pnpm", "yarn", "python", "python3", "rustc", "cargo", "tsc", "oxlint":
		return len(args) == 1 && (args[0] == "-v" || args[0] == "-V" || args[0] == "--version")
	case "get-location", "gl":
		return len(args) == 0
	case "get-childitem", "gci", "dir":
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-recurse": true, "-name": true, "-force": true, "-file": true,
			"-directory": true, "-hidden": true, "-readonly": true, "-system": true,
			"-followsymlink": true,
		}, map[string]bool{
			"-path": true, "-literalpath": true, "-filter": true, "-include": true,
			"-exclude": true, "-depth": true, "-attributes": true,
		})
	case "get-content", "gc", "type":
		// Explicitly excludes -wait so read-only commands never block indefinitely.
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-raw": true, "-readonly": true,
		}, map[string]bool{
			"-path": true, "-literalpath": true, "-totalcount": true, "-head": true,
			"-first": true, "-tail": true, "-last": true, "-encoding": true,
			"-delimiter": true, "-readcount": true, "-filter": true, "-include": true,
			"-exclude": true,
		})
	case "select-string", "sls":
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-casesensitive": true, "-simplematch": true, "-allmatches": true,
			"-list": true, "-quiet": true, "-notmatch": true, "-raw": true,
			"-noemphasis": true,
		}, map[string]bool{
			"-pattern": true, "-path": true, "-literalpath": true, "-context": true,
			"-encoding": true, "-include": true, "-exclude": true, "-culture": true,
		})
	case "test-path":
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-isvalid": true,
		}, map[string]bool{
			"-path": true, "-literalpath": true, "-pathtype": true, "-filter": true,
			"-include": true, "-exclude": true, "-newerthan": true, "-olderthan": true,
		})
	case "resolve-path", "rvpa":
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-relative": true,
		}, map[string]bool{
			"-path": true, "-literalpath": true, "-relativebasepath": true,
		})
	case "get-item", "gi", "get-itemproperty", "gp":
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-force": true,
		}, map[string]bool{
			"-path": true, "-literalpath": true, "-name": true, "-filter": true,
			"-include": true, "-exclude": true,
		})
	case "where":
		// Allow Windows where.exe lookups while rejecting PowerShell Where-Object script blocks.
		return isAllowedWhereArgs(args)
	case "get-command", "gcm":
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-all": true, "-listimported": true,
		}, map[string]bool{
			"-name": true, "-commandtype": true, "-module": true, "-syntax": true,
		})
	case "get-date":
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-asutc": true,
		}, map[string]bool{
			"-format": true, "-uformat": true,
		})
	case "write-output":
		return allPowerShellFlagsAllowed(args, map[string]bool{
			"-noenumerate": true,
		}, map[string]bool{
			"-inputobject": true,
		})
	default:
		return false
	}
}

// allPowerShellFlagsAllowed validates PowerShell cmdlet flags case-insensitively.
// Rejects script blocks ({...}) and supports -Flag value as well as -Flag:value syntax.
func allPowerShellFlagsAllowed(args []string, boolFlags map[string]bool, valueFlags map[string]bool) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.ContainsAny(arg, "{}") {
			return false
		}
		if arg == "--" {
			return true
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		lower := strings.ToLower(arg)
		if colonIdx := strings.IndexByte(lower, ':'); colonIdx > 0 {
			flagName := lower[:colonIdx]
			if boolFlags[flagName] || valueFlags[flagName] {
				continue
			}
			return false
		}
		if boolFlags[lower] {
			continue
		}
		if valueFlags[lower] {
			if i+1 >= len(args) {
				return false
			}
			i++
			if strings.ContainsAny(args[i], "{}") {
				return false
			}
			continue
		}
		return false
	}
	return true
}

func isAllowedWhereArgs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.ContainsAny(arg, "{}") {
			return false
		}
		if strings.HasPrefix(arg, "-") {
			return false
		}
		if strings.HasPrefix(arg, "/") {
			switch strings.ToLower(arg) {
			case "/q", "/f", "/t":
				continue
			case "/r":
				if i+1 >= len(args) {
					return false
				}
				i++
				continue
			default:
				return false
			}
		}
	}
	return true
}

func isAllowedHeadTailArgs(args []string) bool {
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		// Support POSIX shorthand like "tail -2" or "head -20".
		if len(arg) >= 2 && arg[0] == '-' && isDigits(arg[1:]) {
			continue
		}
		filtered = append(filtered, arg)
	}
	return allFlagsAllowed(filtered, nil, map[string]bool{"-n": true, "-c": true})
}

func isAllowedLsArgs(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		if strings.HasPrefix(arg, "--") {
			if arg != "--all" && arg != "--almost-all" && arg != "--human-readable" {
				return false
			}
			continue
		}
		for _, ch := range arg[1:] {
			if !strings.ContainsRune("laAhhrt1dFGSR", ch) {
				return false
			}
		}
	}
	return true
}

func isAllowedFindArgs(args []string) bool {
	allowedUnary := map[string]bool{
		"-not": true, "-and": true, "-or": true, "-a": true, "-o": true,
		"-print": true, "-print0": true, "-empty": true, "-L": true, "-P": true,
	}
	allowedValue := map[string]bool{
		"-name": true, "-iname": true, "-path": true, "-ipath": true,
		"-type": true, "-maxdepth": true, "-mindepth": true, "-size": true,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		if allowedUnary[arg] {
			continue
		}
		if allowedValue[arg] {
			i++
			continue
		}
		return false
	}
	return true
}

func isAllowedGoArgs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version":
		return allFlagsAllowed(args[1:], map[string]bool{"-m": true, "-v": true}, nil)
	case "env":
		// Explicitly disallow -w / -u which mutate Go environment config.
		return allFlagsAllowed(args[1:], map[string]bool{"-json": true}, nil)
	case "doc":
		return allFlagsAllowed(args[1:], map[string]bool{"-all": true, "-short": true, "-u": true, "-src": true}, nil)
	case "list":
		return allFlagsAllowed(args[1:], map[string]bool{"-m": true, "-json": true, "-deps": true}, map[string]bool{"-f": true})
	default:
		return false
	}
}

func isAllowedGitArgs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	// Reject global flags before subcommand (such as -c, --config-env, --git-dir, --work-tree)
	// except --version.
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		return true
	}
	if strings.HasPrefix(args[0], "-") {
		return false
	}

	sub := args[0]
	rest := args[1:]
	switch sub {
	case "status":
		return allFlagsAllowed(rest, map[string]bool{
			"-s": true, "--short": true, "-b": true, "--branch": true,
			"--porcelain": true, "-u": true, "--untracked-files": true,
		}, map[string]bool{"--untracked-files": true})
	case "log":
		return allFlagsAllowed(rest, map[string]bool{
			"--oneline": true, "--stat": true, "--graph": true, "--decorate": true,
			"--all": true, "-p": true, "--patch": true, "--name-only": true,
			"--name-status": true, "--no-merges": true, "--reverse": true,
		}, map[string]bool{
			"-n": true, "--max-count": true, "--format": true, "--pretty": true,
			"--since": true, "--until": true, "--author": true, "--grep": true,
		})
	case "diff":
		// Explicitly excludes --output and --ext-diff.
		return allFlagsAllowed(rest, map[string]bool{
			"--stat": true, "--name-only": true, "--name-status": true,
			"--cached": true, "--staged": true, "--numstat": true,
			"--shortstat": true, "--no-color": true, "--color": true,
			"--no-ext-diff": true,
		}, map[string]bool{
			"-U": true, "--unified": true,
		})
	case "show":
		return allFlagsAllowed(rest, map[string]bool{
			"--stat": true, "--name-only": true, "--name-status": true,
			"--oneline": true, "--no-patch": true, "-s": true,
		}, map[string]bool{
			"--format": true, "--pretty": true,
		})
	case "branch":
		// Only allow listing/querying flags; reject positional args (which create branches) and -d/-D/-m/-M.
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				return false
			}
			switch a {
			case "-a", "--all", "-r", "--remotes", "-v", "-vv", "--list", "--show-current":
			default:
				return false
			}
		}
		return true
	case "rev-parse":
		return allFlagsAllowed(rest, map[string]bool{
			"--abbrev-ref": true, "--short": true, "--show-toplevel": true,
			"--is-inside-work-tree": true, "--git-dir": true,
		}, nil)
	case "remote":
		return len(rest) == 0 || (len(rest) == 1 && (rest[0] == "-v" || rest[0] == "--verbose"))
	case "tag":
		if len(rest) == 0 {
			return true
		}
		return len(rest) >= 1 && (rest[0] == "-l" || rest[0] == "--list") && allFlagsAllowed(rest[1:], nil, nil)
	default:
		return false
	}
}

// allFlagsAllowed verifies every flag argument is in either boolFlags or valueFlags.
// Supports --flag=value form for valueFlags and stops checking flags after "--".
func allFlagsAllowed(args []string, boolFlags map[string]bool, valueFlags map[string]bool) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return true
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		if eqIdx := strings.IndexByte(arg, '='); eqIdx > 0 {
			flagName := arg[:eqIdx]
			if valueFlags[flagName] {
				continue
			}
			return false
		}
		if boolFlags[arg] {
			continue
		}
		if valueFlags[arg] {
			if i+1 >= len(args) {
				return false
			}
			i++
			continue
		}
		// Allow compact numeric forms like -n5 or -U3 when -n / -U is a valueFlag,
		// and combined short boolean flags like -rn when every individual flag is in boolFlags.
		if len(arg) > 2 && arg[0] == '-' && arg[1] != '-' {
			shortFlag := arg[:2]
			if valueFlags[shortFlag] && isDigits(arg[2:]) {
				continue
			}
			allShortBool := true
			for _, ch := range arg[1:] {
				if !boolFlags["-"+string(ch)] {
					allShortBool = false
					break
				}
			}
			if allShortBool {
				continue
			}
		}
		return false
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// splitShellWords splits a single shell command line respecting single/double quotes and backslash escapes.
// Returns (nil, false) on unterminated quotes or trailing escape.
func splitShellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	inToken := false
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			inToken = true
			escaped = false
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		if inDouble {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inDouble = false
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\\':
			escaped = true
			inToken = true
		case '\'':
			inSingle = true
			inToken = true
		case '"':
			inDouble = true
			inToken = true
		case ' ', '\t':
			if inToken {
				words = append(words, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			cur.WriteByte(c)
			inToken = true
		}
	}
	if inSingle || inDouble || escaped {
		return nil, false
	}
	if inToken {
		words = append(words, cur.String())
	}
	return words, true
}
