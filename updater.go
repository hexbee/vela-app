package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

// UpdateState reflects the current update status, replicating deepseek-harness update phases
type UpdateState struct {
	Phase   string `json:"phase"`   // "idle" | "checking" | "available" | "downloading" | "verifying" | "ready" | "error"
	Version string `json:"version"` // Latest version string
	Percent int    `json:"percent"` // Download progress (0-100)
	Notes   string `json:"notes"`   // Release notes
	Error   string `json:"error"`   // Localized or technical error message
}

type UpdateService struct {
	mu            sync.RWMutex
	state         UpdateState
	currentUpdate *mygo.Update
	downloading   bool
}

var globalUpdateService = &UpdateService{
	state: UpdateState{Phase: "idle"},
}

func (s *UpdateService) GetUpdateStatus() UpdateState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *UpdateService) setPhase(phase string, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Phase = phase
	s.state.Error = err
}

func (s *UpdateService) setProgress(percent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Percent = percent
}

// CheckUpdate checks whether a newer version is available.
func (s *UpdateService) CheckUpdate() (UpdateState, error) {
	s.mu.Lock()
	s.state.Phase = "checking"
	s.state.Error = ""
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 1. Try native MyGo Updater first if enabled
	if mygo.Updater.Enabled() {
		up, err := mygo.Updater.Check(ctx)
		if err != nil {
			s.setPhase("error", fmt.Sprintf("检查更新失败: %v", err))
			return s.GetUpdateStatus(), err
		}
		if up != nil {
			s.mu.Lock()
			s.currentUpdate = up
			s.state.Phase = "available"
			s.state.Version = up.Version
			s.state.Notes = up.Notes
			s.state.Percent = 0
			s.mu.Unlock()
			return s.GetUpdateStatus(), nil
		}
		// No update available
		s.setPhase("idle", "")
		return s.GetUpdateStatus(), nil
	}

	// 2. Fallback / Dev mode: check GitHub Releases API
	latestVersion, notes, assetURL, err := checkGitHubRelease(ctx, "hexbee/vela-app")
	if err != nil {
		s.setPhase("error", fmt.Sprintf("检查更新失败: %v", err))
		return s.GetUpdateStatus(), err
	}

	if isVersionNewer(latestVersion, CurrentAppVersion) {
		s.mu.Lock()
		s.state.Phase = "available"
		s.state.Version = latestVersion
		s.state.Notes = notes
		s.state.Percent = 0
		s.mu.Unlock()
		_ = assetURL
		return s.GetUpdateStatus(), nil
	}

	s.setPhase("idle", "")
	return s.GetUpdateStatus(), nil
}

// StartDownload begins downloading the update payload with progress
func (s *UpdateService) StartDownload() (UpdateState, error) {
	s.mu.Lock()
	if s.downloading {
		s.mu.Unlock()
		return s.GetUpdateStatus(), nil
	}
	if s.state.Phase != "available" && s.state.Phase != "error" {
		s.mu.Unlock()
		return s.GetUpdateStatus(), fmt.Errorf("no update available to download")
	}
	s.downloading = true
	s.state.Phase = "downloading"
	s.state.Percent = 0
	s.state.Error = ""
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.downloading = false
			s.mu.Unlock()
		}()

		ctx := context.Background()

		if mygo.Updater.Enabled() && s.currentUpdate != nil {
			err := s.currentUpdate.Install(ctx, func(downloaded, total int64) {
				if total > 0 {
					pct := int(float64(downloaded) / float64(total) * 100)
					s.setProgress(pct)
				}
			})
			if err != nil {
				s.setPhase("error", fmt.Sprintf("下载更新失败: %v", err))
				return
			}
			s.setPhase("ready", "")
			return
		}

		// Fallback downloader when native update signature infrastructure isn't configured in dev
		latestVersion, _, assetURL, err := checkGitHubRelease(ctx, "hexbee/vela-app")
		if err != nil || assetURL == "" {
			s.setPhase("error", "未找到当前平台对应的安装包资产")
			return
		}

		tmpDir, err := os.UserCacheDir()
		if err != nil {
			tmpDir = os.TempDir()
		}
		targetFile := filepath.Join(tmpDir, fmt.Sprintf("vela-update-%s-%s-%s", latestVersion, runtime.GOOS, runtime.GOARCH))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
		if err != nil {
			s.setPhase("error", err.Error())
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			s.setPhase("error", err.Error())
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			s.setPhase("error", fmt.Sprintf("下载失败: HTTP %d", resp.StatusCode))
			return
		}

		out, err := os.Create(targetFile)
		if err != nil {
			s.setPhase("error", err.Error())
			return
		}
		defer out.Close()

		total := resp.ContentLength
		var downloaded int64
		buf := make([]byte, 32*1024)
		for {
			n, rErr := resp.Body.Read(buf)
			if n > 0 {
				_, wErr := out.Write(buf[:n])
				if wErr != nil {
					s.setPhase("error", wErr.Error())
					return
				}
				downloaded += int64(n)
				if total > 0 {
					s.setProgress(int(float64(downloaded) / float64(total) * 100))
				}
			}
			if rErr == io.EOF {
				break
			}
			if rErr != nil {
				s.setPhase("error", rErr.Error())
				return
			}
		}

		s.setPhase("verifying", "")
		time.Sleep(300 * time.Millisecond) // Smooth transition
		s.setPhase("ready", "")
	}()

	return s.GetUpdateStatus(), nil
}

// Relaunch restarts the application to apply the downloaded update
func (s *UpdateService) Relaunch() (bool, error) {
	s.mu.RLock()
	ready := s.state.Phase == "ready"
	s.mu.RUnlock()

	if !ready {
		return false, fmt.Errorf("update is not ready yet")
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		mygo.App.Relaunch()
	}()
	return true, nil
}

// DismissUpdate closes the update alert/state
func (s *UpdateService) DismissUpdate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Phase = "idle"
	return true
}

type gitHubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type gitHubRelease struct {
	TagName string               `json:"tag_name"`
	Body    string               `json:"body"`
	Assets  []gitHubReleaseAsset `json:"assets"`
}

func matchSuffixRank(name string, suffixes []string) int {
	for i, s := range suffixes {
		if strings.HasSuffix(name, s) {
			return i
		}
	}
	return -1
}

func platformAssetExtensions(goos string) (exclusive []string, shared []string) {
	switch goos {
	case "darwin":
		return []string{".dmg"}, []string{".tar.gz", ".zip"}
	case "windows":
		return []string{".exe", ".msi"}, []string{".zip", ".tar.gz"}
	case "linux":
		return []string{".deb", ".rpm", ".appimage"}, []string{".tar.gz", ".zip"}
	default:
		return nil, nil
	}
}

func matchesArch(lowerName, goarch string) bool {
	switch goarch {
	case "amd64":
		return strings.Contains(lowerName, "amd64") || strings.Contains(lowerName, "x86_64") || strings.Contains(lowerName, "x64")
	case "arm64":
		return strings.Contains(lowerName, "arm64") || strings.Contains(lowerName, "aarch64")
	default:
		return strings.Contains(lowerName, goarch)
	}
}

func mentionsOtherOS(lowerName, goos string) bool {
	for _, other := range []string{"darwin", "macos", "windows", "linux"} {
		if other == goos || (goos == "darwin" && other == "macos") {
			continue
		}
		if strings.Contains(lowerName, other) {
			return true
		}
	}
	return false
}

// selectGitHubReleaseAsset picks the best fallback release asset for goos/goarch.
// Naming contract (aligned with `mygo build` outputs):
//   - Asset filenames must include an architecture token (amd64/x86_64/x64 or arm64/aarch64);
//     universal or arch-less bundles (e.g. an arch-less .dmg) are intentionally not matched.
//   - Explicit OS matching recognizes goos (plus "macos" for darwin) and rejects names
//     mentioning another OS among darwin/macos/windows/linux; aliases like "osx"/"mac" are not matched.
//   - Tier 1 (explicit OS + arch) beats Tier 2 (OS-exclusive extension + arch); within the same
//     tier, earlier extensions in platformAssetExtensions win (installers before generic archives).
func selectGitHubReleaseAsset(assets []gitHubReleaseAsset, goos, goarch string) string {
	goos = strings.ToLower(strings.TrimSpace(goos))
	goarch = strings.ToLower(strings.TrimSpace(goarch))
	exclusiveExts, sharedExts := platformAssetExtensions(goos)
	if goos == "" || goarch == "" || (len(exclusiveExts) == 0 && len(sharedExts) == 0) {
		return ""
	}

	bestURL := ""
	bestTier := 0
	bestRank := 0

	for _, a := range assets {
		lower := strings.ToLower(strings.TrimSpace(a.Name))
		if lower == "" {
			continue
		}

		isExclusive := false
		extRank := matchSuffixRank(lower, exclusiveExts)
		if extRank >= 0 {
			isExclusive = true
		} else {
			sharedRank := matchSuffixRank(lower, sharedExts)
			if sharedRank < 0 {
				continue
			}
			extRank = len(exclusiveExts) + sharedRank
		}

		if !matchesArch(lower, goarch) || mentionsOtherOS(lower, goos) {
			continue
		}

		var tier int
		if strings.Contains(lower, goos) || (goos == "darwin" && strings.Contains(lower, "macos")) {
			// Tier 1: explicit OS + arch match (prefer platform-exclusive installers over generic archives).
			tier = 1
		} else if isExclusive {
			// Tier 2: OS-exclusive extension + arch match (e.g. vela-setup-0.3.5-amd64.exe, vela-0.3.5-arm64.dmg).
			tier = 2
		} else {
			continue
		}

		if bestURL == "" || tier < bestTier || (tier == bestTier && extRank < bestRank) {
			bestURL = a.BrowserDownloadURL
			bestTier = tier
			bestRank = extRank
		}
	}

	return bestURL
}

func checkGitHubRelease(ctx context.Context, repo string) (string, string, string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "vela-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var rel gitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", "", err
	}

	version := strings.TrimPrefix(rel.TagName, "v")
	targetAsset := selectGitHubReleaseAsset(rel.Assets, runtime.GOOS, runtime.GOARCH)

	return version, rel.Body, targetAsset, nil
}

func isVersionNewer(remote, current string) bool {
	rParts := parseSemver(remote)
	cParts := parseSemver(current)
	for i := 0; i < 3; i++ {
		if rParts[i] > cParts[i] {
			return true
		}
		if rParts[i] < cParts[i] {
			return false
		}
	}
	return false
}

func parseSemver(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.Split(v, ".")
	var res [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		n, _ := strconv.Atoi(parts[i])
		res[i] = n
	}
	return res
}
