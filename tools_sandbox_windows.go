//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// Win32 ACL / Restricted Token constants matching Windows SDK headers.
const (
	winTokenAssignPrimary = 0x0001
	winTokenDuplicate     = 0x0002
	winTokenQuery         = 0x0008
	winTokenAdjustDefault = 0x0080

	winTokenGroups         = 2
	winTokenDefaultDacl    = 6
	winTokenIntegrityLevel = 25

	winSeGroupLogonID   uint32 = 0xC0000000
	winSeGroupIntegrity uint32 = 0x00000020

	winDisableMaxPrivilege uint32 = 0x1
	winWriteRestricted     uint32 = 0x8

	winWorldSid    uint32 = 1
	winLowLabelSid uint32 = 66

	winSecurityMaxSidSize = 68

	winStandardRightsWrite uint32 = 0x00020000
	winFileGenericWrite    uint32 = 0x00120116
	winDeleteRight         uint32 = 0x00010000
	winFileDeleteChild     uint32 = 0x0040
	winFileAllAccess       uint32 = 0x001F01FF

	// Capability-SID access mask granting write, delete, and child deletion while
	// excluding WRITE_DAC and WRITE_OWNER so a confined child cannot alter ACLs.
	winGrantMask uint32 = (winFileGenericWrite | winDeleteRight | winFileDeleteChild) &^ winStandardRightsWrite

	winDACLSecurityInfo  uint32 = 0x00000004
	winLabelSecurityInfo uint32 = 0x00000010

	winSeFileObject uint32 = 1

	winGrantAccess  uint32 = 1
	winDenyAccess   uint32 = 3
	winRevokeAccess uint32 = 4

	winContainerInheritAce            uint32 = 0x2
	winSubContainersAndObjectsInherit uint32 = 0x3

	winTrusteeIsSid     uint32 = 0
	winTrusteeIsUnknown uint32 = 0
	winNoMultiple       uint32 = 0

	winAccessAllowedAceType       byte   = 0x00
	winAccessDeniedAceType        byte   = 0x01
	winSystemMandatoryLabelAce    byte   = 0x11
	winSystemMandatoryNoWriteUp   uint32 = 0x00000001
	winACLRevision                uint32 = 2
	winACLHeaderSize                     = 8
	winMandatoryAceOverhead              = 8
	winLPTR                       uint32 = 0x0040
	winLockfileExclusiveLock      uint32 = 0x2
)

var (
	advapi32DLL                = syscall.NewLazyDLL("advapi32.dll")
	procCreateRestrictedToken  = advapi32DLL.NewProc("CreateRestrictedToken")
	procSetTokenInformation    = advapi32DLL.NewProc("SetTokenInformation")
	procCreateWellKnownSid     = advapi32DLL.NewProc("CreateWellKnownSid")
	procIsValidSid             = advapi32DLL.NewProc("IsValidSid")
	procGetLengthSid           = advapi32DLL.NewProc("GetLengthSid")
	procCopySid                = advapi32DLL.NewProc("CopySid")
	procEqualSid               = advapi32DLL.NewProc("EqualSid")
	procConvertStringSidToSidW = advapi32DLL.NewProc("ConvertStringSidToSidW")
	procGetNamedSecurityInfoW  = advapi32DLL.NewProc("GetNamedSecurityInfoW")
	procSetNamedSecurityInfoW  = advapi32DLL.NewProc("SetNamedSecurityInfoW")
	procSetEntriesInAclW       = advapi32DLL.NewProc("SetEntriesInAclW")
	procInitializeAcl          = advapi32DLL.NewProc("InitializeAcl")
	procAddMandatoryAce        = advapi32DLL.NewProc("AddMandatoryAce")

	kernel32DLL               = syscall.NewLazyDLL("kernel32.dll")
	procLocalAlloc            = kernel32DLL.NewProc("LocalAlloc")
	procLockFileEx            = kernel32DLL.NewProc("LockFileEx")
	procUnlockFileEx          = kernel32DLL.NewProc("UnlockFileEx")
	procGetVolumeInformationW = kernel32DLL.NewProc("GetVolumeInformationW")
)

type winSidAndAttributes struct {
	Sid        uintptr
	Attributes uint32
}

type winTokenGroupsStruct struct {
	GroupCount uint32
	Groups     [1]winSidAndAttributes
}

type winTokenMandatoryLabel struct {
	Label winSidAndAttributes
}

type winTokenDefaultDaclStruct struct {
	DefaultDacl uintptr
}

type winTrusteeW struct {
	MultipleTrustee          *winTrusteeW
	MultipleTrusteeOperation uint32
	TrusteeForm              uint32
	TrusteeType              uint32
	Name                     uintptr
}

type winExplicitAccessW struct {
	AccessPermissions uint32
	AccessMode        uint32
	Inheritance       uint32
	Trustee           winTrusteeW
}

var (
	winSandboxProbeOnce sync.Once
	winSandboxSupported bool
)

// isOSSandboxAvailable probes whether the current Windows host supports creating
// a WRITE_RESTRICTED + Low integrity token with an extended default DACL.
// The probe performs no filesystem or ACL mutations.
func isOSSandboxAvailable() bool {
	winSandboxProbeOnce.Do(func() {
		tok, err := createWinRestrictedToken(PermissionReadOnly, nil, 0)
		if err == nil && tok != 0 {
			_ = tok.Close()
			winSandboxSupported = true
		}
	})
	return winSandboxSupported
}

// buildSandboxedShellCommand provides compatibility with callers expecting the
// legacy two-value signature. Prefer prepareSandboxedShellCommand on Windows
// so token handles and temporary ACL grants are properly cleaned up.
func buildSandboxedShellCommand(command string, spec sandboxSpec) (*exec.Cmd, bool) {
	prepared, err := prepareSandboxedShellCommand(command, spec, nil)
	if err != nil {
		return buildShellCommand(command), false
	}
	return prepared.cmd, prepared.sandboxed
}

type winSessionTempEntry struct {
	key            string
	sessionID      string
	dir            string
	sidStr         string
	activeCount    int
	pendingCleanup bool
}

var (
	winSessionTempMu   sync.Mutex
	winSessionTempPool = make(map[string]*winSessionTempEntry)
)

func cleanupWinTempDir(dir, sidStr string) {
	if dir == "" {
		return
	}
	if sidStr != "" {
		if sidPtr, err := parseWinStringSid(sidStr); err == nil && sidPtr != 0 {
			_ = revokeWinWrite(dir, sidPtr)
			_, _ = syscall.LocalFree(syscall.Handle(sidPtr))
		}
	}
	_ = os.RemoveAll(dir)
}

// acquireWinPrivateTempDir obtains a private temp directory + capability SID for a
// workspace-write sandboxed command. When sessionID is non-empty, the directory is
// reused across commands for the same (sessionID, workspaceRoots) pair with reference
// counting and revoked on session release or app exit.
func acquireWinPrivateTempDir(
	sessionID string,
	roots []string,
	lowLabelSid []byte,
	worldSid []byte,
) (dir string, sidStr string, sidPtr uintptr, release func(), err error) {
	cacheKey := windowsSessionTempKey(sessionID, roots)
	if cacheKey != "" {
		winSessionTempMu.Lock()
		if entry, ok := winSessionTempPool[cacheKey]; ok && !entry.pendingCleanup {
			if info, statErr := os.Stat(entry.dir); statErr == nil && info.IsDir() {
				parsed, parseErr := parseWinStringSid(entry.sidStr)
				if parseErr == nil && parsed != 0 {
					entry.activeCount++
					winSessionTempMu.Unlock()
					var once sync.Once
					rel := func() {
						once.Do(func() {
							releaseWinSessionTempEntry(entry)
						})
					}
					return entry.dir, entry.sidStr, parsed, rel, nil
				}
			}
			// Stale/missing dir: evict from active pool and fall through to create a fresh one.
			delete(winSessionTempPool, cacheKey)
			entry.pendingCleanup = true
		}
		winSessionTempMu.Unlock()
	}

	tmpDir, tmpErr := os.MkdirTemp("", "vela-sbx-*")
	if tmpErr != nil {
		return "", "", 0, nil, fmt.Errorf("create private sandbox temp dir: %w", tmpErr)
	}
	if canonical, cErr := canonicalizePath(tmpDir); cErr == nil {
		tmpDir = canonical
	}

	for _, root := range roots {
		if isWindowsDirOverlap(root, tmpDir) {
			_ = os.RemoveAll(tmpDir)
			return "", "", 0, nil, fmt.Errorf("private temp dir %q overlaps workspace root %q", tmpDir, root)
		}
	}

	tSidStr := windowsTempWriteSid(tmpDir)
	parsedTempSid, parseErr := parseWinStringSid(tSidStr)
	if parseErr != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", 0, nil, fmt.Errorf("parse temp SID %s: %w", tSidStr, parseErr)
	}

	if grantErr := grantWinWrite(tmpDir, parsedTempSid, uintptr(unsafe.Pointer(&lowLabelSid[0])), uintptr(unsafe.Pointer(&worldSid[0]))); grantErr != nil {
		_ = revokeWinWrite(tmpDir, parsedTempSid)
		_, _ = syscall.LocalFree(syscall.Handle(parsedTempSid))
		_ = os.RemoveAll(tmpDir)
		return "", "", 0, nil, fmt.Errorf("grant temp ACL on %q: %w", tmpDir, grantErr)
	}

	if cacheKey == "" {
		// Ephemeral per-command temp dir: revoke and remove immediately on command exit.
		var once sync.Once
		rel := func() {
			once.Do(func() {
				cleanupWinTempDir(tmpDir, tSidStr)
			})
		}
		return tmpDir, tSidStr, parsedTempSid, rel, nil
	}

	entry := &winSessionTempEntry{
		key:         cacheKey,
		sessionID:   sessionID,
		dir:         tmpDir,
		sidStr:      tSidStr,
		activeCount: 1,
	}
	winSessionTempMu.Lock()
	if existing, ok := winSessionTempPool[cacheKey]; ok && !existing.pendingCleanup {
		// Another goroutine won the race to populate this session key; reuse it and clean up our extra dir.
		existing.activeCount++
		winSessionTempMu.Unlock()
		_, _ = syscall.LocalFree(syscall.Handle(parsedTempSid))
		cleanupWinTempDir(tmpDir, tSidStr)
		existingSidPtr, pErr := parseWinStringSid(existing.sidStr)
		if pErr != nil || existingSidPtr == 0 {
			releaseWinSessionTempEntry(existing)
			return "", "", 0, nil, fmt.Errorf("parse pooled temp SID %s: %w", existing.sidStr, pErr)
		}
		var once sync.Once
		rel := func() {
			once.Do(func() {
				releaseWinSessionTempEntry(existing)
			})
		}
		return existing.dir, existing.sidStr, existingSidPtr, rel, nil
	}
	winSessionTempPool[cacheKey] = entry
	winSessionTempMu.Unlock()

	var once sync.Once
	rel := func() {
		once.Do(func() {
			releaseWinSessionTempEntry(entry)
		})
	}
	return tmpDir, tSidStr, parsedTempSid, rel, nil
}

func releaseWinSessionTempEntry(entry *winSessionTempEntry) {
	if entry == nil {
		return
	}
	var shouldCleanup bool
	winSessionTempMu.Lock()
	if entry.activeCount > 0 {
		entry.activeCount--
	}
	if entry.pendingCleanup && entry.activeCount == 0 {
		shouldCleanup = true
	}
	winSessionTempMu.Unlock()
	if shouldCleanup {
		cleanupWinTempDir(entry.dir, entry.sidStr)
	}
}

func releaseSessionSandboxTemp(sessionID string) {
	if sessionID == "" {
		return
	}
	var toCleanup []*winSessionTempEntry
	winSessionTempMu.Lock()
	for key, entry := range winSessionTempPool {
		if entry.sessionID != sessionID {
			continue
		}
		delete(winSessionTempPool, key)
		entry.pendingCleanup = true
		if entry.activeCount == 0 {
			toCleanup = append(toCleanup, entry)
		}
	}
	winSessionTempMu.Unlock()
	for _, entry := range toCleanup {
		cleanupWinTempDir(entry.dir, entry.sidStr)
	}
}

func releaseAllSandboxTemps() {
	var toCleanup []*winSessionTempEntry
	winSessionTempMu.Lock()
	for key, entry := range winSessionTempPool {
		delete(winSessionTempPool, key)
		entry.pendingCleanup = true
		if entry.activeCount == 0 {
			toCleanup = append(toCleanup, entry)
		}
	}
	winSessionTempMu.Unlock()
	for _, entry := range toCleanup {
		cleanupWinTempDir(entry.dir, entry.sidStr)
	}
}

// prepareSandboxedShellCommand prepares a Windows WRITE_RESTRICTED + Low integrity
// shell command. Under workspace-write mode it applies idempotent standing ACL grants
// on the workspace roots and acquires a session-scoped (or per-command) private temp
// dir (rewriting TMP/TEMP). Fails closed on any Win32 preparation error.
func prepareSandboxedShellCommand(command string, spec sandboxSpec, baseEnv []string) (sandboxedShellCommand, error) {
	if spec.mode == PermissionFullAccess || !isOSSandboxAvailable() {
		cmd := buildShellCommand(command)
		if baseEnv != nil {
			cmd.Env = baseEnv
		}
		return sandboxedShellCommand{
			cmd:       cmd,
			sandboxed: false,
		}, nil
	}

	lowLabelSid, err := makeWinWellKnownSid(winLowLabelSid)
	if err != nil {
		return sandboxedShellCommand{}, fmt.Errorf("create Low integrity SID: %w", err)
	}
	worldSid, err := makeWinWellKnownSid(winWorldSid)
	if err != nil {
		return sandboxedShellCommand{}, fmt.Errorf("create Everyone SID: %w", err)
	}

	var (
		writeSidPtrs   []uintptr
		tempSidPtr     uintptr
		privateTempDir string
		releaseTemp    func()
	)
	freeParsedSids := func() {
		for _, ptr := range writeSidPtrs {
			if ptr != 0 {
				_, _ = syscall.LocalFree(syscall.Handle(ptr))
			}
		}
		writeSidPtrs = nil
		if tempSidPtr != 0 {
			_, _ = syscall.LocalFree(syscall.Handle(tempSidPtr))
			tempSidPtr = 0
		}
	}
	failCleanup := func() {
		freeParsedSids()
		if releaseTemp != nil {
			releaseTemp()
			releaseTemp = nil
		}
	}

	if spec.mode == PermissionWorkspaceWrite {
		roots := spec.workspaceRoots
		if len(roots) == 0 {
			failCleanup()
			return sandboxedShellCommand{}, errors.New("workspace-write sandbox requires at least one workspace root")
		}

		// Apply standing workspace ACL grants (idempotent O(1) skip when already present).
		seenSid := make(map[string]bool)
		for _, root := range roots {
			info, statErr := os.Stat(root)
			if statErr != nil || !info.IsDir() {
				failCleanup()
				return sandboxedShellCommand{}, fmt.Errorf("workspace root %q is not an accessible directory", root)
			}
			if volErr := checkWindowsVolumeSupportsACLs(root); volErr != nil {
				failCleanup()
				return sandboxedShellCommand{}, volErr
			}
			sidStr := windowsWorkspaceWriteSid(root)
			if seenSid[sidStr] {
				continue
			}
			sidPtr, parseErr := parseWinStringSid(sidStr)
			if parseErr != nil {
				failCleanup()
				return sandboxedShellCommand{}, fmt.Errorf("parse workspace SID %s: %w", sidStr, parseErr)
			}
			writeSidPtrs = append(writeSidPtrs, sidPtr)
			seenSid[sidStr] = true

			if grantErr := grantWinWrite(root, sidPtr, uintptr(unsafe.Pointer(&lowLabelSid[0])), uintptr(unsafe.Pointer(&worldSid[0]))); grantErr != nil {
				failCleanup()
				return sandboxedShellCommand{}, fmt.Errorf("grant workspace ACL on %q: %w", root, grantErr)
			}
		}

		var tempErr error
		privateTempDir, _, tempSidPtr, releaseTemp, tempErr = acquireWinPrivateTempDir(spec.sessionID, roots, lowLabelSid, worldSid)
		if tempErr != nil {
			failCleanup()
			return sandboxedShellCommand{}, tempErr
		}
	}

	restrictedToken, err := createWinRestrictedToken(spec.mode, writeSidPtrs, tempSidPtr)
	runtime.KeepAlive(lowLabelSid)
	runtime.KeepAlive(worldSid)
	if err != nil {
		failCleanup()
		return sandboxedShellCommand{}, fmt.Errorf("create restricted token: %w", err)
	}
	freeParsedSids()

	cmd := buildShellCommand(command)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = restrictedToken
	cmd.SysProcAttr.HideWindow = true
	// Clear CREATE_NO_WINDOW: restricted Low-IL processes crash with STATUS_DLL_INIT_FAILED (0xC0000142)
	// when spawned with CREATE_NO_WINDOW or CREATE_NEW_CONSOLE.
	cmd.SysProcAttr.CreationFlags &^= createNoWindow

	if baseEnv != nil {
		if privateTempDir != "" {
			cmd.Env = overrideEnvVars(baseEnv, map[string]string{
				"TMP":  privateTempDir,
				"TEMP": privateTempDir,
			})
		} else {
			cmd.Env = baseEnv
		}
	}

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if restrictedToken != 0 {
				_ = restrictedToken.Close()
			}
			if releaseTemp != nil {
				releaseTemp()
			}
		})
	}

	return sandboxedShellCommand{
		cmd:       cmd,
		sandboxed: true,
		cleanup:   cleanup,
	}, nil
}

func createWinRestrictedToken(mode string, workspaceSidPtrs []uintptr, tempSidPtr uintptr) (syscall.Token, error) {
	procHandle, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, fmt.Errorf("GetCurrentProcess: %w", err)
	}

	var currentToken syscall.Token
	if err := syscall.OpenProcessToken(
		procHandle,
		winTokenQuery|winTokenDuplicate|winTokenAdjustDefault|winTokenAssignPrimary,
		&currentToken,
	); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer currentToken.Close()

	logonSid, err := findWinLogonSid(currentToken)
	if err != nil {
		return 0, err
	}
	worldSid, err := makeWinWellKnownSid(winWorldSid)
	if err != nil {
		return 0, err
	}
	lowLabelSid, err := makeWinWellKnownSid(winLowLabelSid)
	if err != nil {
		return 0, err
	}

	restricting := make([]winSidAndAttributes, 0, 2+len(workspaceSidPtrs)+1)
	restricting = append(restricting,
		winSidAndAttributes{Sid: uintptr(unsafe.Pointer(&logonSid[0]))},
		winSidAndAttributes{Sid: uintptr(unsafe.Pointer(&worldSid[0]))},
	)
	if mode == PermissionWorkspaceWrite {
		if len(workspaceSidPtrs) == 0 {
			return 0, errors.New("workspace-write restricting list requires at least one workspace SID")
		}
		for _, sidPtr := range workspaceSidPtrs {
			restricting = append(restricting, winSidAndAttributes{Sid: sidPtr})
		}
		if tempSidPtr != 0 {
			restricting = append(restricting, winSidAndAttributes{Sid: tempSidPtr})
		}
	}

	var restrictedToken syscall.Token
	r1, _, callErr := procCreateRestrictedToken.Call(
		uintptr(currentToken),
		uintptr(winDisableMaxPrivilege|winWriteRestricted),
		0, 0,
		0, 0,
		uintptr(len(restricting)),
		uintptr(unsafe.Pointer(&restricting[0])),
		uintptr(unsafe.Pointer(&restrictedToken)),
	)
	runtime.KeepAlive(logonSid)
	runtime.KeepAlive(worldSid)
	runtime.KeepAlive(restricting)
	if r1 == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}
	if restrictedToken == 0 {
		return 0, errors.New("CreateRestrictedToken returned null token")
	}

	if err := setWinTokenLowIntegrity(restrictedToken, lowLabelSid); err != nil {
		_ = restrictedToken.Close()
		return 0, err
	}

	// Extend the restricted token's default DACL with a restricting SID so anonymous
	// pipes created by the child (and its grandchildren) pass the write pass-2 check.
	defaultDaclSid := uintptr(unsafe.Pointer(&worldSid[0]))
	if mode == PermissionWorkspaceWrite {
		if tempSidPtr != 0 {
			defaultDaclSid = tempSidPtr
		} else if len(workspaceSidPtrs) > 0 {
			defaultDaclSid = workspaceSidPtrs[0]
		}
	}
	if err := setWinTokenDefaultDaclGrant(restrictedToken, defaultDaclSid); err != nil {
		runtime.KeepAlive(worldSid)
		_ = restrictedToken.Close()
		return 0, err
	}
	runtime.KeepAlive(worldSid)
	runtime.KeepAlive(lowLabelSid)
	return restrictedToken, nil
}

func findWinLogonSid(token syscall.Token) ([]byte, error) {
	var needed uint32
	_ = syscall.GetTokenInformation(token, winTokenGroups, nil, 0, &needed)
	if needed == 0 {
		return nil, errors.New("GetTokenInformation(TokenGroups) returned 0 size")
	}
	buf := make([]byte, needed)
	if err := syscall.GetTokenInformation(token, winTokenGroups, &buf[0], needed, &needed); err != nil {
		return nil, fmt.Errorf("GetTokenInformation(TokenGroups): %w", err)
	}

	tg := (*winTokenGroupsStruct)(unsafe.Pointer(&buf[0]))
	groups := unsafe.Slice(&tg.Groups[0], tg.GroupCount)
	for _, g := range groups {
		if g.Sid == 0 || (g.Attributes&winSeGroupLogonID) != winSeGroupLogonID {
			continue
		}
		sidLen, _, _ := procGetLengthSid.Call(g.Sid)
		if sidLen == 0 {
			return nil, errors.New("GetLengthSid failed for logon SID")
		}
		out := make([]byte, sidLen)
		r1, _, callErr := procCopySid.Call(sidLen, uintptr(unsafe.Pointer(&out[0])), g.Sid)
		if r1 == 0 {
			return nil, fmt.Errorf("CopySid(logon): %w", callErr)
		}
		runtime.KeepAlive(buf)
		return out, nil
	}
	runtime.KeepAlive(buf)
	return nil, fmt.Errorf("no logon SID found among %d token groups", tg.GroupCount)
}

func makeWinWellKnownSid(sidType uint32) ([]byte, error) {
	buf := make([]byte, winSecurityMaxSidSize)
	size := uint32(len(buf))
	r1, _, callErr := procCreateWellKnownSid.Call(
		uintptr(sidType),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("CreateWellKnownSid(%d): %w", sidType, callErr)
	}
	valid, _, _ := procIsValidSid.Call(uintptr(unsafe.Pointer(&buf[0])))
	if valid == 0 {
		return nil, fmt.Errorf("IsValidSid(%d) returned false", sidType)
	}
	return buf[:size], nil
}

func parseWinStringSid(sidStr string) (uintptr, error) {
	u16, err := syscall.UTF16PtrFromString(sidStr)
	if err != nil {
		return 0, err
	}
	var sidPtr uintptr
	r1, _, callErr := procConvertStringSidToSidW.Call(
		uintptr(unsafe.Pointer(u16)),
		uintptr(unsafe.Pointer(&sidPtr)),
	)
	runtime.KeepAlive(u16)
	if r1 == 0 || sidPtr == 0 {
		return 0, fmt.Errorf("ConvertStringSidToSidW(%s): %w", sidStr, callErr)
	}
	return sidPtr, nil
}

func setWinTokenLowIntegrity(token syscall.Token, lowLabelSid []byte) error {
	sidLen, _, _ := procGetLengthSid.Call(uintptr(unsafe.Pointer(&lowLabelSid[0])))
	if sidLen == 0 {
		return errors.New("GetLengthSid(Low) returned 0")
	}
	tml := winTokenMandatoryLabel{
		Label: winSidAndAttributes{
			Sid:        uintptr(unsafe.Pointer(&lowLabelSid[0])),
			Attributes: winSeGroupIntegrity,
		},
	}
	infoLen := uint32(unsafe.Sizeof(tml)) + uint32(sidLen)
	r1, _, callErr := procSetTokenInformation.Call(
		uintptr(token),
		uintptr(winTokenIntegrityLevel),
		uintptr(unsafe.Pointer(&tml)),
		uintptr(infoLen),
	)
	runtime.KeepAlive(lowLabelSid)
	runtime.KeepAlive(&tml)
	if r1 == 0 {
		return fmt.Errorf("SetTokenInformation(TokenIntegrityLevel): %w", callErr)
	}
	return nil
}

func setWinTokenDefaultDaclGrant(token syscall.Token, sidPtr uintptr) error {
	var needed uint32
	_ = syscall.GetTokenInformation(token, winTokenDefaultDacl, nil, 0, &needed)
	if needed == 0 {
		return errors.New("GetTokenInformation(TokenDefaultDacl) returned 0 size")
	}
	buf := make([]byte, needed)
	if err := syscall.GetTokenInformation(token, winTokenDefaultDacl, &buf[0], needed, &needed); err != nil {
		return fmt.Errorf("GetTokenInformation(TokenDefaultDacl): %w", err)
	}
	cur := (*winTokenDefaultDaclStruct)(unsafe.Pointer(&buf[0]))
	if cur.DefaultDacl == 0 {
		return errors.New("token carries no default DACL")
	}

	ea := buildWinExplicitAccess(sidPtr, winGrantAccess, winFileAllAccess, winSubContainersAndObjectsInherit)
	var newDacl uintptr
	res, _, _ := procSetEntriesInAclW.Call(
		1,
		uintptr(unsafe.Pointer(&ea)),
		cur.DefaultDacl,
		uintptr(unsafe.Pointer(&newDacl)),
	)
	runtime.KeepAlive(buf)
	runtime.KeepAlive(&ea)
	if res != 0 || newDacl == 0 {
		return fmt.Errorf("SetEntriesInAclW(default DACL): error %d", res)
	}
	defer syscall.LocalFree(syscall.Handle(newDacl))

	info := winTokenDefaultDaclStruct{DefaultDacl: newDacl}
	r1, _, callErr := procSetTokenInformation.Call(
		uintptr(token),
		uintptr(winTokenDefaultDacl),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	runtime.KeepAlive(&info)
	if r1 == 0 {
		return fmt.Errorf("SetTokenInformation(TokenDefaultDacl): %w", callErr)
	}
	return nil
}

func buildWinExplicitAccess(sidPtr uintptr, mode, permissions, inheritance uint32) winExplicitAccessW {
	return winExplicitAccessW{
		AccessPermissions: permissions,
		AccessMode:        mode,
		Inheritance:       inheritance,
		Trustee: winTrusteeW{
			MultipleTrusteeOperation: winNoMultiple,
			TrusteeForm:              winTrusteeIsSid,
			TrusteeType:              winTrusteeIsUnknown,
			Name:                     sidPtr,
		},
	}
}

func withWinPathLock(path string, fn func() error) error {
	lockDir := filepath.Join(os.TempDir(), "vela-acl-locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return fmt.Errorf("create ACL lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, windowsPathLockKey(path)+".lock")
	u16, err := syscall.UTF16PtrFromString(lockPath)
	if err != nil {
		return err
	}
	handle, err := syscall.CreateFile(
		u16,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_ALWAYS,
		0,
		0,
	)
	runtime.KeepAlive(u16)
	if err != nil {
		return fmt.Errorf("CreateFile(%s): %w", lockPath, err)
	}

	var overlapped syscall.Overlapped
	r1, _, callErr := procLockFileEx.Call(
		uintptr(handle),
		uintptr(winLockfileExclusiveLock),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		_ = syscall.CloseHandle(handle)
		return fmt.Errorf("LockFileEx(%s): %w", lockPath, callErr)
	}

	fnErr := fn()

	_, _, _ = procUnlockFileEx.Call(
		uintptr(handle),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	runtime.KeepAlive(&overlapped)
	_ = syscall.CloseHandle(handle)
	return fnErr
}

func readWinSecurity(path string) (oldAcl, labelAcl unsafe.Pointer, descriptor uintptr, err error) {
	u16, uErr := syscall.UTF16PtrFromString(path)
	if uErr != nil {
		return nil, nil, 0, uErr
	}
	var owner, group uintptr
	res, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(u16)),
		uintptr(winSeFileObject),
		uintptr(winDACLSecurityInfo|winLabelSecurityInfo),
		uintptr(unsafe.Pointer(&owner)),
		uintptr(unsafe.Pointer(&group)),
		uintptr(unsafe.Pointer(&oldAcl)),
		uintptr(unsafe.Pointer(&labelAcl)),
		uintptr(unsafe.Pointer(&descriptor)),
	)
	runtime.KeepAlive(u16)
	if res != 0 {
		return nil, nil, 0, fmt.Errorf("GetNamedSecurityInfoW(%s): error %d", path, res)
	}
	return oldAcl, labelAcl, descriptor, nil
}

func buildWinLowLabelAcl(lowLabelSidPtr uintptr) (uintptr, error) {
	sidLen, _, _ := procGetLengthSid.Call(lowLabelSidPtr)
	if sidLen == 0 {
		return 0, errors.New("GetLengthSid(LowLabel) returned 0")
	}
	aclLen := uintptr(winACLHeaderSize+winMandatoryAceOverhead) + sidLen
	aclPtr, _, callErr := procLocalAlloc.Call(uintptr(winLPTR), aclLen)
	if aclPtr == 0 {
		return 0, fmt.Errorf("LocalAlloc(LowLabelAcl): %w", callErr)
	}
	r1, _, callErr := procInitializeAcl.Call(aclPtr, aclLen, uintptr(winACLRevision))
	if r1 == 0 {
		_, _ = syscall.LocalFree(syscall.Handle(aclPtr))
		return 0, fmt.Errorf("InitializeAcl: %w", callErr)
	}
	r1, _, callErr = procAddMandatoryAce.Call(
		aclPtr,
		uintptr(winACLRevision),
		uintptr(winSubContainersAndObjectsInherit),
		uintptr(winSystemMandatoryNoWriteUp),
		lowLabelSidPtr,
	)
	if r1 == 0 {
		_, _ = syscall.LocalFree(syscall.Handle(aclPtr))
		return 0, fmt.Errorf("AddMandatoryAce: %w", callErr)
	}
	return aclPtr, nil
}

func hasWinExactEntry(aclPtr unsafe.Pointer, aceType byte, aceFlags byte, mask uint32, sidPtr uintptr) bool {
	if aclPtr == nil || sidPtr == 0 {
		return false
	}
	hdr := unsafe.Slice((*byte)(aclPtr), winACLHeaderSize)
	aclSize := int(binary.LittleEndian.Uint16(hdr[2:4]))
	aceCount := int(binary.LittleEndian.Uint16(hdr[4:6]))
	if aclSize < winACLHeaderSize || aclSize > 1_048_576 {
		return false
	}
	buf := unsafe.Slice((*byte)(aclPtr), aclSize)
	offset := winACLHeaderSize
	for i := 0; i < aceCount; i++ {
		if offset+8 > aclSize {
			return false
		}
		aceSize := int(binary.LittleEndian.Uint16(buf[offset+2 : offset+4]))
		if aceSize < 8 || offset+aceSize > aclSize {
			return false
		}
		if buf[offset] == aceType &&
			buf[offset+1] == aceFlags &&
			binary.LittleEndian.Uint32(buf[offset+4:offset+8]) == mask {
			if offset+8 < aclSize {
				eq, _, _ := procEqualSid.Call(uintptr(unsafe.Pointer(&buf[offset+8])), sidPtr)
				if eq != 0 {
					return true
				}
			}
		}
		offset += aceSize
	}
	return false
}

func hasWinForeignGrant(aclPtr unsafe.Pointer, sidPtr uintptr) bool {
	if aclPtr == nil {
		return false
	}
	hdr := unsafe.Slice((*byte)(aclPtr), winACLHeaderSize)
	aclSize := int(binary.LittleEndian.Uint16(hdr[2:4]))
	aceCount := int(binary.LittleEndian.Uint16(hdr[4:6]))
	if aclSize < winACLHeaderSize || aclSize > 1_048_576 {
		return false
	}
	buf := unsafe.Slice((*byte)(aclPtr), aclSize)
	offset := winACLHeaderSize
	for i := 0; i < aceCount; i++ {
		if offset+8 > aclSize {
			return false
		}
		aceSize := int(binary.LittleEndian.Uint16(buf[offset+2 : offset+4]))
		if aceSize < 8 || offset+aceSize > aclSize {
			return false
		}
		if buf[offset] == winAccessAllowedAceType &&
			binary.LittleEndian.Uint32(buf[offset+4:offset+8]) == winGrantMask {
			if offset+8 < aclSize {
				eq, _, _ := procEqualSid.Call(uintptr(unsafe.Pointer(&buf[offset+8])), sidPtr)
				if eq == 0 {
					return true
				}
			}
		}
		offset += aceSize
	}
	return false
}

func grantWinWrite(path string, sidPtr, lowLabelSidPtr, worldSidPtr uintptr) error {
	return withWinPathLock(path, func() error {
		oldAcl, labelAcl, descriptor, err := readWinSecurity(path)
		if err != nil {
			return err
		}
		if oldAcl != nil && labelAcl != nil &&
			hasWinExactEntry(oldAcl, winAccessAllowedAceType, byte(winSubContainersAndObjectsInherit), winGrantMask, sidPtr) &&
			hasWinExactEntry(oldAcl, winAccessDeniedAceType, byte(winContainerInheritAce), winFileDeleteChild, worldSidPtr) &&
			hasWinExactEntry(labelAcl, winSystemMandatoryLabelAce, byte(winSubContainersAndObjectsInherit), winSystemMandatoryNoWriteUp, lowLabelSidPtr) {
			if descriptor != 0 {
				_, _ = syscall.LocalFree(syscall.Handle(descriptor))
			}
			return nil
		}

		newLabelAcl, err := buildWinLowLabelAcl(lowLabelSidPtr)
		if err != nil {
			if descriptor != 0 {
				_, _ = syscall.LocalFree(syscall.Handle(descriptor))
			}
			return err
		}

		entries := []winExplicitAccessW{
			buildWinExplicitAccess(worldSidPtr, winDenyAccess, winFileDeleteChild, winContainerInheritAce),
			buildWinExplicitAccess(sidPtr, winGrantAccess, winGrantMask, winSubContainersAndObjectsInherit),
		}
		var newAcl uintptr
		res, _, _ := procSetEntriesInAclW.Call(
			uintptr(len(entries)),
			uintptr(unsafe.Pointer(&entries[0])),
			uintptr(oldAcl),
			uintptr(unsafe.Pointer(&newAcl)),
		)
		runtime.KeepAlive(entries)
		if descriptor != 0 {
			_, _ = syscall.LocalFree(syscall.Handle(descriptor))
		}
		if res != 0 || newAcl == 0 {
			_, _ = syscall.LocalFree(syscall.Handle(newLabelAcl))
			return fmt.Errorf("SetEntriesInAclW(grant %s): error %d", path, res)
		}
		defer syscall.LocalFree(syscall.Handle(newAcl))
		defer syscall.LocalFree(syscall.Handle(newLabelAcl))

		u16, uErr := syscall.UTF16PtrFromString(path)
		if uErr != nil {
			return uErr
		}
		applyRes, _, _ := procSetNamedSecurityInfoW.Call(
			uintptr(unsafe.Pointer(u16)),
			uintptr(winSeFileObject),
			uintptr(winDACLSecurityInfo|winLabelSecurityInfo),
			0,
			0,
			newAcl,
			newLabelAcl,
		)
		runtime.KeepAlive(u16)
		if applyRes != 0 {
			return errors.New(formatWindowsACLSandboxError("SetNamedSecurityInfoW(grant)", path, applyRes))
		}
		return nil
	})
}

func revokeWinWrite(path string, sidPtr uintptr) error {
	return withWinPathLock(path, func() error {
		oldAcl, _, descriptor, err := readWinSecurity(path)
		if err != nil {
			return err
		}
		if oldAcl == nil {
			if descriptor != 0 {
				_, _ = syscall.LocalFree(syscall.Handle(descriptor))
			}
			return nil
		}

		keepLabel := hasWinForeignGrant(oldAcl, sidPtr)
		entry := buildWinExplicitAccess(sidPtr, winRevokeAccess, 0, winSubContainersAndObjectsInherit)
		var newAcl uintptr
		res, _, _ := procSetEntriesInAclW.Call(
			1,
			uintptr(unsafe.Pointer(&entry)),
			uintptr(oldAcl),
			uintptr(unsafe.Pointer(&newAcl)),
		)
		runtime.KeepAlive(&entry)
		if descriptor != 0 {
			_, _ = syscall.LocalFree(syscall.Handle(descriptor))
		}
		if res != 0 || newAcl == 0 {
			return fmt.Errorf("SetEntriesInAclW(revoke %s): error %d", path, res)
		}
		defer syscall.LocalFree(syscall.Handle(newAcl))

		secInfo := winDACLSecurityInfo
		if !keepLabel {
			secInfo |= winLabelSecurityInfo
		}
		u16, uErr := syscall.UTF16PtrFromString(path)
		if uErr != nil {
			return uErr
		}
		applyRes, _, _ := procSetNamedSecurityInfoW.Call(
			uintptr(unsafe.Pointer(u16)),
			uintptr(winSeFileObject),
			uintptr(secInfo),
			0,
			0,
			newAcl,
			0,
		)
		runtime.KeepAlive(u16)
		if applyRes != 0 {
			return errors.New(formatWindowsACLSandboxError("SetNamedSecurityInfoW(revoke)", path, applyRes))
		}
		return nil
	})
}

const winFilePersistentACLs uint32 = 0x00000008

// checkWindowsVolumeSupportsACLs checks whether the volume hosting path supports
// persistent NTFS ACLs (rejecting FAT32/exFAT with a clear message before ACL calls).
func checkWindowsVolumeSupportsACLs(path string) error {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return nil
	}
	rootPath := vol + "\\"
	u16, err := syscall.UTF16PtrFromString(rootPath)
	if err != nil {
		return nil
	}
	var fsFlags uint32
	fsNameBuf := make([]uint16, 64)
	r1, _, _ := procGetVolumeInformationW.Call(
		uintptr(unsafe.Pointer(u16)),
		0,
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&fsFlags)),
		uintptr(unsafe.Pointer(&fsNameBuf[0])),
		uintptr(len(fsNameBuf)),
	)
	runtime.KeepAlive(u16)
	runtime.KeepAlive(fsNameBuf)
	if r1 == 0 {
		// If volume metadata cannot be queried (e.g., some virtual/network roots),
		// let the subsequent ACL call fail-closed with its exact Win32 error.
		return nil
	}
	if fsFlags&winFilePersistentACLs == 0 {
		fsName := syscall.UTF16ToString(fsNameBuf)
		if fsName == "" {
			fsName = "non-NTFS"
		}
		return fmt.Errorf("Windows ACL sandbox requires an NTFS volume with persistent ACLs, but %q is on %s (unsupported); move the workspace to an NTFS drive or switch to full-access mode", path, fsName)
	}
	return nil
}

// cleanupWorkspaceSandboxACL revokes the standing workspace capability ACE (and clears
// the Low mandatory label if no other capability grant remains) on workspaceRoot.
func cleanupWorkspaceSandboxACL(workspaceRoot string) (bool, error) {
	trimmed := filepath.Clean(workspaceRoot)
	if trimmed == "" || trimmed == "." {
		return false, errors.New("workspace path is required")
	}
	if canonical, err := canonicalizePath(trimmed); err == nil {
		trimmed = canonical
	}
	info, err := os.Stat(trimmed)
	if err != nil || !info.IsDir() {
		return false, fmt.Errorf("workspace %q is not an accessible directory", trimmed)
	}
	sidStr := windowsWorkspaceWriteSid(trimmed)
	sidPtr, err := parseWinStringSid(sidStr)
	if err != nil {
		return false, err
	}
	defer syscall.LocalFree(syscall.Handle(sidPtr))

	oldAcl, _, descriptor, err := readWinSecurity(trimmed)
	if err != nil {
		return false, err
	}
	hasGrant := oldAcl != nil && hasWinExactEntry(oldAcl, winAccessAllowedAceType, byte(winSubContainersAndObjectsInherit), winGrantMask, sidPtr)
	if descriptor != 0 {
		_, _ = syscall.LocalFree(syscall.Handle(descriptor))
	}
	if !hasGrant {
		return false, nil
	}
	if err := revokeWinWrite(trimmed, sidPtr); err != nil {
		return false, err
	}
	return true, nil
}
