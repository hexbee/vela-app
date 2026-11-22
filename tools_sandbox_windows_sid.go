package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const winCapabilitySidMod uint32 = (1 << 30) - 1

// windowsWorkspaceWriteSid derives a deterministic per-workspace capability SID
// ("S-1-4-<first>-<second>") from the canonical workspace path so standing
// workspace ACL grants materialize once per workspace per machine and subsequent
// invocations hit the exact-ACE idempotent skip.
func windowsWorkspaceWriteSid(canonicalWorkspaceRoot string) string {
	normalized := strings.ToLower(filepath.Clean(strings.TrimSpace(canonicalWorkspaceRoot)))
	sum := sha256.Sum256([]byte(normalized))
	first := (binary.LittleEndian.Uint32(sum[0:4]) % winCapabilitySidMod) + 1
	second := (binary.LittleEndian.Uint32(sum[4:8]) % winCapabilitySidMod) + 1
	return fmt.Sprintf("S-1-4-%d-%d", first, second)
}

// windowsTempWriteSid derives a distinct capability SID ("S-1-4-<first>-<second>-1")
// for a private per-command/session temp directory. The third subauthority domain-separates
// temp SIDs from two-subauthority workspace SIDs.
func windowsTempWriteSid(tempDir string) string {
	normalized := strings.ToLower(filepath.Clean(strings.TrimSpace(tempDir)))
	h := sha256.New()
	_, _ = h.Write([]byte("temp\x00"))
	_, _ = h.Write([]byte(normalized))
	sum := h.Sum(nil)
	first := (binary.LittleEndian.Uint32(sum[0:4]) % winCapabilitySidMod) + 1
	second := (binary.LittleEndian.Uint32(sum[4:8]) % winCapabilitySidMod) + 1
	return fmt.Sprintf("S-1-4-%d-%d-1", first, second)
}

// windowsPathLockKey computes a stable 16-hex-char lock filename stem for a directory path.
func windowsPathLockKey(path string) string {
	normalized := strings.ToLower(filepath.Clean(strings.TrimSpace(path)))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

// overrideEnvVars returns a copy of env with each key in overrides replaced case-insensitively
// (or appended if absent).
func overrideEnvVars(env []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		out := make([]string, len(env))
		copy(out, env)
		return out
	}
	lowerOverrides := make(map[string]string, len(overrides))
	canonicalNames := make(map[string]string, len(overrides))
	for k, v := range overrides {
		lk := strings.ToLower(k)
		lowerOverrides[lk] = v
		canonicalNames[lk] = k
	}

	seen := make(map[string]bool, len(overrides))
	out := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		key, _, hasEq := strings.Cut(entry, "=")
		if !hasEq {
			out = append(out, entry)
			continue
		}
		lk := strings.ToLower(key)
		if val, ok := lowerOverrides[lk]; ok {
			if !seen[lk] {
				out = append(out, canonicalNames[lk]+"="+val)
				seen[lk] = true
			}
			continue
		}
		out = append(out, entry)
	}

	var missing []string
	for lk := range lowerOverrides {
		if !seen[lk] {
			missing = append(missing, lk)
		}
	}
	sort.Strings(missing)
	for _, lk := range missing {
		out = append(out, canonicalNames[lk]+"="+lowerOverrides[lk])
	}
	return out
}

// isWindowsDirOverlap reports whether two canonical directory paths are equal
// or one contains the other (case-insensitive for Windows paths).
func isWindowsDirOverlap(a, b string) bool {
	ca := strings.ToLower(filepath.Clean(strings.TrimSpace(a)))
	cb := strings.ToLower(filepath.Clean(strings.TrimSpace(b)))
	if ca == "" || cb == "" {
		return false
	}
	if ca == cb {
		return true
	}
	return isPathWithinFold(ca, cb) || isPathWithinFold(cb, ca)
}

func isPathWithinFold(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// windowsSessionTempKey computes a stable cache key for a (sessionID, workspaceRoots) pair.
// Returns "" when sessionID is empty so non-session calls use an ephemeral per-command temp dir.
func windowsSessionTempKey(sessionID string, workspaceRoots []string) string {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return ""
	}
	normalized := make([]string, 0, len(workspaceRoots))
	seen := make(map[string]bool, len(workspaceRoots))
	for _, root := range workspaceRoots {
		cleaned := strings.ToLower(filepath.Clean(strings.TrimSpace(root)))
		if cleaned == "" || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		normalized = append(normalized, cleaned)
	}
	sort.Strings(normalized)
	h := sha256.New()
	_, _ = h.Write([]byte(sid))
	for _, root := range normalized {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(root))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// formatWindowsACLSandboxError translates common Win32 ACL / mandatory-label error codes
// into actionable human-readable guidance for NTFS, FAT/exFAT, network share, and permission failures.
func formatWindowsACLSandboxError(api, path string, code uintptr) string {
	switch code {
	case 5, 1314: // ERROR_ACCESS_DENIED, ERROR_PRIVILEGE_NOT_HELD
		return fmt.Sprintf("%s(%s) failed (Win32 error %d): current user lacks WRITE_DAC/WRITE_OWNER permission on this directory; ensure the workspace is owned by your user account with Full Control, or switch to full-access mode", api, path, code)
	case 1, 50, 53, 67, 87: // ERROR_INVALID_FUNCTION, ERROR_NOT_SUPPORTED, ERROR_BAD_NETPATH, ERROR_BAD_NET_NAME, ERROR_INVALID_PARAMETER
		return fmt.Sprintf("%s(%s) failed (Win32 error %d): Windows ACL sandbox requires a local NTFS volume that supports persistent ACLs and mandatory integrity labels (FAT32/exFAT and some network shares are unsupported)", api, path, code)
	case 32, 33: // ERROR_SHARING_VIOLATION, ERROR_LOCK_VIOLATION
		return fmt.Sprintf("%s(%s) failed (Win32 error %d): another process is holding an exclusive lock on this path; retry once the lock is released", api, path, code)
	default:
		return fmt.Sprintf("%s(%s) failed with Win32 error %d", api, path, code)
	}
}
