package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitignoreMatcherRules(t *testing.T) {
	root := t.TempDir()

	writeFile := func(rel, content string) string {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		return full
	}

	writeFile(".gitignore", ""+
		"# comment\n"+
		"*.log\n"+
		"!important.log\n"+
		"build/\n"+
		"/root_only.txt\n"+
		"data/**/cache.bin\n",
	)
	writeFile("sub/.gitignore", "*.tmp\n!keep.tmp\n")

	m := newGitignoreMatcher(root)
	m.loadDir(filepath.Join(root, "sub"))

	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"debug.log", false, true},
		{"sub/app.log", false, true},
		{"important.log", false, false},
		{"build", true, true},
		{"build/out.js", false, true},
		{"build", false, false}, // dirOnly does not ignore a file named "build"
		{"root_only.txt", false, true},
		{"sub/root_only.txt", false, false}, // anchored to root
		{"data/cache.bin", false, true},
		{"data/a/b/cache.bin", false, true},
		{"sub/a.tmp", false, true},
		{"sub/keep.tmp", false, false},
		{"a.tmp", false, false}, // sub/.gitignore only applies under sub/
		{"main.py", false, false},
	}

	for _, tc := range cases {
		got := m.isIgnored(filepath.Join(root, filepath.FromSlash(tc.rel)), tc.isDir)
		if got != tc.want {
			t.Errorf("isIgnored(%q, isDir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}

func TestBashSnapshotRespectsGitignore(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	if err := os.WriteFile(
		filepath.Join(wsDir, ".gitignore"),
		[]byte("*.log\n!keep.log\nignored_dir/\n"),
		0o644,
	); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}

	cmd := "mkdir -p ignored_dir && echo 'a' > ignored_dir/a.txt && echo 'b' > debug.log && echo 'c' > keep.log && echo 'd' > main.py"
	res, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "gitignore-snap-1",
		ToolName:       "bash",
		Workdir:        wsDir,
		PermissionMode: PermissionWorkspaceWrite,
		Approved:       true,
		Input:          map[string]any{"command": cmd},
	})
	if err != nil || res.IsError {
		t.Fatalf("bash failed: err=%v res=%#v", err, res)
	}

	produced, _ := res.UIMeta["producedFiles"].([]string)
	if len(produced) != 2 || filepath.Base(produced[0]) != "keep.log" || filepath.Base(produced[1]) != "main.py" {
		t.Fatalf("expected [keep.log main.py], got %#v", produced)
	}
}

func TestGlobAndGrepRespectGitignore(t *testing.T) {
	ts := &ToolService{}
	wsDir, _ := setupTestWorkspace(t)

	writeFile := func(rel, content string) {
		full := filepath.Join(wsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	writeFile(".gitignore", "*.log\n!keep.log\nignored_dir/\n")
	writeFile("debug.log", "NEEDLE in ignored log\n")
	writeFile("keep.log", "NEEDLE in kept log\n")
	writeFile("ignored_dir/secret.txt", "NEEDLE in ignored dir\n")
	writeFile("src/app.py", "NEEDLE in source\n")

	// 1. glob across workspace skips debug.log and ignored_dir/secret.txt, keeps keep.log and src/app.py.
	globRes, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "gitignore-glob-1",
		ToolName:       "glob",
		Workdir:        wsDir,
		PermissionMode: PermissionReadOnly,
		Input:          map[string]any{"pattern": "**/*"},
	})
	if err != nil || globRes.IsError {
		t.Fatalf("glob failed: err=%v res=%#v", err, globRes)
	}
	for _, forbidden := range []string{"debug.log", "ignored_dir/secret.txt"} {
		if strings.Contains(globRes.ModelContent, forbidden) {
			t.Fatalf("glob should exclude %q, got:\n%s", forbidden, globRes.ModelContent)
		}
	}
	for _, expected := range []string{"keep.log", "src/app.py"} {
		if !strings.Contains(globRes.ModelContent, expected) {
			t.Fatalf("glob should include %q, got:\n%s", expected, globRes.ModelContent)
		}
	}

	// 2. grep across workspace skips ignored files/dirs.
	grepRes, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "gitignore-grep-1",
		ToolName:       "grep",
		Workdir:        wsDir,
		PermissionMode: PermissionReadOnly,
		Input:          map[string]any{"pattern": "NEEDLE"},
	})
	if err != nil || grepRes.IsError {
		t.Fatalf("grep failed: err=%v res=%#v", err, grepRes)
	}
	for _, forbidden := range []string{"debug.log", "ignored_dir/secret.txt"} {
		if strings.Contains(grepRes.ModelContent, forbidden) {
			t.Fatalf("grep should exclude %q, got:\n%s", forbidden, grepRes.ModelContent)
		}
	}
	for _, expected := range []string{"keep.log", "src/app.py"} {
		if !strings.Contains(grepRes.ModelContent, expected) {
			t.Fatalf("grep should include %q, got:\n%s", expected, grepRes.ModelContent)
		}
	}

	// 3. Explicitly targeting an ignored directory still allows searching inside it.
	explicitRes, err := ts.ExecuteTool(ToolExecuteRequest{
		SessionID:      "gitignore-grep-2",
		ToolName:       "grep",
		Workdir:        wsDir,
		PermissionMode: PermissionReadOnly,
		Input:          map[string]any{"pattern": "NEEDLE", "path": "ignored_dir"},
	})
	if err != nil || explicitRes.IsError {
		t.Fatalf("explicit grep failed: err=%v res=%#v", err, explicitRes)
	}
	if !strings.Contains(explicitRes.ModelContent, "secret.txt") {
		t.Fatalf("explicit grep inside ignored_dir should find secret.txt, got:\n%s", explicitRes.ModelContent)
	}
}
