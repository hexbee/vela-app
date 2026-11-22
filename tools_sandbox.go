package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// osSandboxAvailableFn is overridden in unit tests to deterministically exercise both
// sandboxed and unsandboxed policy paths regardless of host nesting.
var osSandboxAvailableFn = isOSSandboxAvailable

// sandboxSpec describes the OS-level sandbox policy applied to a bash command.
type sandboxSpec struct {
	sessionID       string // Active session ID for session-scoped private temp reuse (Windows ACL sandbox)
	mode            string // PermissionReadOnly | PermissionWorkspaceWrite | PermissionFullAccess
	writableRoots   []string
	workspaceRoots  []string // Explicit workspace roots without ambient temp dirs (used by Windows ACL sandbox)
	deniedReadPaths []string
}

type sandboxedShellCommand struct {
	cmd       *exec.Cmd
	sandboxed bool
	cleanup   func()
}

func escapeSBPLString(p string) string {
	escaped := strings.ReplaceAll(p, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// buildSeatbeltProfile generates a macOS sandbox-exec SBPL profile:
//   - read-only: denies all file writes except /dev/null, and denies reading sensitive credential dirs
//   - workspace-write: denies all file writes except /dev/null, workspace roots, and temp dirs,
//     and denies reading sensitive credential dirs outside the workspace
func buildSeatbeltProfile(spec sandboxSpec) string {
	forms := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (literal "/dev/null"))`,
	}
	if spec.mode == PermissionWorkspaceWrite && len(spec.writableRoots) > 0 {
		parts := make([]string, 0, len(spec.writableRoots))
		for _, root := range spec.writableRoots {
			parts = append(parts, "(subpath "+escapeSBPLString(root)+")")
		}
		forms = append(forms, "(allow file-write* "+strings.Join(parts, " ")+")")
	}
	if len(spec.deniedReadPaths) > 0 {
		parts := make([]string, 0, len(spec.deniedReadPaths))
		for _, p := range spec.deniedReadPaths {
			parts = append(parts, "(subpath "+escapeSBPLString(p)+")")
		}
		forms = append(forms, "(deny file-read* "+strings.Join(parts, " ")+")")
	}
	return strings.Join(forms, " ")
}

func collectSandboxWritableRoots(mode string, dirs ...string) []string {
	if mode != PermissionWorkspaceWrite {
		return nil
	}
	seen := make(map[string]bool)
	var roots []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		cleaned := filepath.Clean(p)
		if !seen[cleaned] {
			seen[cleaned] = true
			roots = append(roots, cleaned)
		}
		if canonical, err := canonicalizePath(cleaned); err == nil && !seen[canonical] {
			seen[canonical] = true
			roots = append(roots, canonical)
		}
	}

	for _, d := range dirs {
		add(d)
	}
	// Allow standard temp locations so compilers, shells, and test runners work inside workspace-write.
	add(os.TempDir())
	add("/tmp")
	add("/private/tmp")
	return roots
}

func collectSandboxWorkspaceRoots(mode string, dirs ...string) []string {
	if mode != PermissionWorkspaceWrite {
		return nil
	}
	seen := make(map[string]bool)
	var roots []string
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		cleaned := filepath.Clean(d)
		if canonical, err := canonicalizePath(cleaned); err == nil {
			cleaned = canonical
		}
		key := strings.ToLower(cleaned)
		if !seen[key] {
			seen[key] = true
			roots = append(roots, cleaned)
		}
	}
	return roots
}

var sensitiveCredentialRelPaths = []string{
	".ssh",
	".aws",
	".gnupg",
	".kube",
	filepath.Join(".config", "gcloud"),
	".netrc",
	".npmrc",
	".pypirc",
	filepath.Join(".docker", "config.json"),
}

// collectSensitiveReadDenyPaths returns user credential paths to block from reads inside
// the sandbox, skipping any path that overlaps with the active workspace itself.
func collectSensitiveReadDenyPaths(activeDirs ...string) []string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return nil
	}
	var canonicalActive []string
	for _, d := range activeDirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		if c, cErr := canonicalizePath(d); cErr == nil {
			canonicalActive = append(canonicalActive, c)
		}
	}

	seen := make(map[string]bool)
	var denied []string
	add := func(p string) {
		cleaned := filepath.Clean(p)
		for _, active := range canonicalActive {
			if isPathWithin(active, cleaned) || isPathWithin(cleaned, active) {
				return
			}
		}
		if !seen[cleaned] {
			seen[cleaned] = true
			denied = append(denied, cleaned)
		}
	}

	for _, rel := range sensitiveCredentialRelPaths {
		raw := filepath.Join(home, rel)
		add(raw)
		if canonical, cErr := canonicalizePath(raw); cErr == nil {
			add(canonical)
		}
	}
	return denied
}

func looksLikeSandboxDenial(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "read-only file system") ||
		strings.Contains(lower, "access is denied") ||
		(strings.Contains(lower, "access to the path") && strings.Contains(lower, "is denied")) ||
		strings.Contains(lower, "unauthorizedaccessexception") ||
		strings.Contains(lower, "permission denied")
}
