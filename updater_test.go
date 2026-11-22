package main

import "testing"

func TestSelectGitHubReleaseAsset(t *testing.T) {
	assets := []gitHubReleaseAsset{
		{Name: "SHA256SUMS", BrowserDownloadURL: "https://example.com/SHA256SUMS"},
		{Name: "install.sh", BrowserDownloadURL: "https://example.com/install.sh"},
		{Name: "Vela.exe", BrowserDownloadURL: "https://example.com/Vela.exe"},
		{Name: "update-darwin-arm64.json", BrowserDownloadURL: "https://example.com/update-darwin-arm64.json"},
		{Name: "vela-0.3.4-to-0.3.5-darwin-arm64.delta", BrowserDownloadURL: "https://example.com/vela-0.3.4-to-0.3.5-darwin-arm64.delta"},
		{Name: "vela-0.3.5-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example.com/vela-0.3.5-darwin-arm64.tar.gz"},
		{Name: "vela-0.3.5-darwin-amd64.tar.gz", BrowserDownloadURL: "https://example.com/vela-0.3.5-darwin-amd64.tar.gz"},
		{Name: "vela-0.3.5-arm64.dmg", BrowserDownloadURL: "https://example.com/vela-0.3.5-arm64.dmg"},
		{Name: "vela-0.3.5-windows-arm64-setup.exe", BrowserDownloadURL: "https://example.com/vela-0.3.5-windows-arm64-setup.exe"},
		{Name: "vela-setup-0.3.5-amd64.exe", BrowserDownloadURL: "https://example.com/vela-setup-0.3.5-amd64.exe"},
		{Name: "vela-0.3.5-linux-amd64.deb", BrowserDownloadURL: "https://example.com/vela-0.3.5-linux-amd64.deb"},
	}

	tests := []struct {
		name   string
		assets []gitHubReleaseAsset
		goos   string
		goarch string
		want   string
	}{
		{
			name:   "darwin/arm64 skips json and delta and matches exact os-arch",
			assets: assets,
			goos:   "darwin",
			goarch: "arm64",
			want:   "https://example.com/vela-0.3.5-darwin-arm64.tar.gz",
		},
		{
			name:   "darwin/amd64 matches darwin tar.gz",
			assets: assets,
			goos:   "darwin",
			goarch: "amd64",
			want:   "https://example.com/vela-0.3.5-darwin-amd64.tar.gz",
		},
		{
			name: "darwin/arm64 falls back to dmg without goos in filename",
			assets: []gitHubReleaseAsset{
				{Name: "vela-0.3.5-arm64.dmg", BrowserDownloadURL: "https://example.com/vela-0.3.5-arm64.dmg"},
				{Name: "vela-setup-0.3.5-arm64.exe", BrowserDownloadURL: "https://example.com/vela-setup-0.3.5-arm64.exe"},
			},
			goos:   "darwin",
			goarch: "arm64",
			want:   "https://example.com/vela-0.3.5-arm64.dmg",
		},
		{
			name:   "windows/arm64 matches windows setup exe",
			assets: assets,
			goos:   "windows",
			goarch: "arm64",
			want:   "https://example.com/vela-0.3.5-windows-arm64-setup.exe",
		},
		{
			name:   "windows/amd64 matches setup exe without goos and skips darwin tar.gz and bare Vela.exe",
			assets: assets,
			goos:   "windows",
			goarch: "amd64",
			want:   "https://example.com/vela-setup-0.3.5-amd64.exe",
		},
		{
			name:   "linux/amd64 matches linux deb",
			assets: assets,
			goos:   "linux",
			goarch: "amd64",
			want:   "https://example.com/vela-0.3.5-linux-amd64.deb",
		},
		{
			name:   "linux/arm64 returns empty instead of cross-platform darwin package",
			assets: assets,
			goos:   "linux",
			goarch: "arm64",
			want:   "",
		},
		{
			name:   "empty goarch returns empty",
			assets: assets,
			goos:   "darwin",
			goarch: "",
			want:   "",
		},
		{
			name: "only metadata or delta assets returns empty",
			assets: []gitHubReleaseAsset{
				{Name: "update-darwin-arm64.json", BrowserDownloadURL: "https://example.com/update-darwin-arm64.json"},
				{Name: "vela-0.3.4-to-0.3.5-darwin-arm64.delta", BrowserDownloadURL: "https://example.com/vela-0.3.4-to-0.3.5-darwin-arm64.delta"},
			},
			goos:   "darwin",
			goarch: "arm64",
			want:   "",
		},
		{
			name: "darwin/arm64 prefers dmg over tar.gz within Tier 1 regardless of API order",
			assets: []gitHubReleaseAsset{
				{Name: "vela-0.3.5-darwin-arm64.zip", BrowserDownloadURL: "https://example.com/vela-0.3.5-darwin-arm64.zip"},
				{Name: "vela-0.3.5-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example.com/vela-0.3.5-darwin-arm64.tar.gz"},
				{Name: "vela-0.3.5-darwin-arm64.dmg", BrowserDownloadURL: "https://example.com/vela-0.3.5-darwin-arm64.dmg"},
			},
			goos:   "darwin",
			goarch: "arm64",
			want:   "https://example.com/vela-0.3.5-darwin-arm64.dmg",
		},
		{
			name: "linux/amd64 prefers deb over rpm and tar.gz within Tier 1 regardless of API order",
			assets: []gitHubReleaseAsset{
				{Name: "vela-0.3.5-linux-amd64.tar.gz", BrowserDownloadURL: "https://example.com/vela-0.3.5-linux-amd64.tar.gz"},
				{Name: "vela-0.3.5-linux-amd64.rpm", BrowserDownloadURL: "https://example.com/vela-0.3.5-linux-amd64.rpm"},
				{Name: "vela-0.3.5-linux-amd64.deb", BrowserDownloadURL: "https://example.com/vela-0.3.5-linux-amd64.deb"},
			},
			goos:   "linux",
			goarch: "amd64",
			want:   "https://example.com/vela-0.3.5-linux-amd64.deb",
		},
		{
			name: "windows/amd64 prefers exe over msi within Tier 2 regardless of API order",
			assets: []gitHubReleaseAsset{
				{Name: "vela-setup-0.3.5-amd64.msi", BrowserDownloadURL: "https://example.com/vela-setup-0.3.5-amd64.msi"},
				{Name: "vela-setup-0.3.5-amd64.exe", BrowserDownloadURL: "https://example.com/vela-setup-0.3.5-amd64.exe"},
			},
			goos:   "windows",
			goarch: "amd64",
			want:   "https://example.com/vela-setup-0.3.5-amd64.exe",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectGitHubReleaseAsset(tc.assets, tc.goos, tc.goarch)
			if got != tc.want {
				t.Fatalf("selectGitHubReleaseAsset(%q, %q) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
			}
		})
	}
}
