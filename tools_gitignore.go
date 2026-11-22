package main

import (
	"os"
	"path/filepath"
	"strings"
)

type gitignoreRule struct {
	baseDir  string // slash-separated path relative to matcher root ("" for root)
	pattern  string // normalized slash-separated pattern
	negated  bool
	dirOnly  bool
	anchored bool
}

// gitignoreMatcher evaluates .gitignore and .git/info/exclude rules for a directory tree.
// It supports nested .gitignore files loaded lazily during directory traversal.
type gitignoreMatcher struct {
	root        string // canonical base root for rule relative paths (git repo root if found, else scan root)
	rules       []gitignoreRule
	loadedDirs  map[string]bool
	hasAnyRules bool
}

func newGitignoreMatcher(scanDir string) *gitignoreMatcher {
	cleanScan := filepath.Clean(scanDir)
	repoRoot := findGitRepoRoot(cleanScan)
	root := cleanScan
	if repoRoot != "" {
		root = repoRoot
	}

	m := &gitignoreMatcher{
		root:       root,
		loadedDirs: make(map[string]bool),
	}

	// 1. Load .git/info/exclude if inside a git repository.
	if repoRoot != "" {
		m.loadIgnoreFile(filepath.Join(repoRoot, ".git", "info", "exclude"), "")
	}

	// 2. Load .gitignore files along the chain from root down to scanDir.
	relScan, err := filepath.Rel(root, cleanScan)
	if err == nil && relScan != "." && !strings.HasPrefix(relScan, "..") {
		m.loadDir(root)
		cur := root
		for _, seg := range strings.Split(filepath.ToSlash(relScan), "/") {
			if seg == "" || seg == "." {
				continue
			}
			cur = filepath.Join(cur, seg)
			m.loadDir(cur)
		}
	} else {
		m.loadDir(root)
	}

	return m
}

func findGitRepoRoot(dir string) string {
	cur := filepath.Clean(dir)
	for {
		gitPath := filepath.Join(cur, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// loadDir loads <dir>/.gitignore if present and not yet loaded.
func (m *gitignoreMatcher) loadDir(dir string) {
	if m == nil {
		return
	}
	cleanDir := filepath.Clean(dir)
	if m.loadedDirs[cleanDir] {
		return
	}
	m.loadedDirs[cleanDir] = true

	rel, err := filepath.Rel(m.root, cleanDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	baseDir := ""
	if rel != "." {
		baseDir = filepath.ToSlash(rel)
	}
	m.loadIgnoreFile(filepath.Join(cleanDir, ".gitignore"), baseDir)
}

func (m *gitignoreMatcher) loadIgnoreFile(filePath, baseDir string) {
	data, err := os.ReadFile(filePath)
	if err != nil || len(data) == 0 {
		return
	}
	for _, rawLine := range strings.Split(string(data), "\n") {
		if rule, ok := parseGitignoreLine(rawLine, baseDir); ok {
			m.rules = append(m.rules, rule)
			m.hasAnyRules = true
		}
	}
}

func parseGitignoreLine(rawLine, baseDir string) (gitignoreRule, bool) {
	line := strings.TrimSuffix(rawLine, "\r")
	line = trimUnescapedTrailingSpaces(line)
	if line == "" {
		return gitignoreRule{}, false
	}

	// Comments start with unescaped '#'.
	if strings.HasPrefix(line, "#") {
		return gitignoreRule{}, false
	}

	negated := false
	if strings.HasPrefix(line, "!") {
		negated = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
	}

	if line == "" {
		return gitignoreRule{}, false
	}

	dirOnly := false
	if strings.HasSuffix(line, "/") {
		dirOnly = true
		line = strings.TrimRight(line, "/")
		if line == "" {
			return gitignoreRule{}, false
		}
	}

	anchored := false
	if strings.HasPrefix(line, "/") {
		anchored = true
		line = strings.TrimLeft(line, "/")
	} else if strings.Contains(line, "/") {
		anchored = true
	}

	if line == "" {
		return gitignoreRule{}, false
	}

	return gitignoreRule{
		baseDir:  baseDir,
		pattern:  line,
		negated:  negated,
		dirOnly:  dirOnly,
		anchored: anchored,
	}, true
}

func trimUnescapedTrailingSpaces(s string) string {
	end := len(s)
	for end > 0 && s[end-1] == ' ' {
		// Count consecutive backslashes immediately before this space.
		bs := 0
		for i := end - 2; i >= 0 && s[i] == '\\'; i-- {
			bs++
		}
		if bs%2 == 1 {
			// Escaped trailing space: keep the space and drop the escaping backslash.
			return s[:end-2] + " "
		}
		end--
	}
	return s[:end]
}

// isIgnored reports whether absPath (a file or directory under matcher root) is ignored.
func (m *gitignoreMatcher) isIgnored(absPath string, isDir bool) bool {
	if m == nil || !m.hasAnyRules {
		return false
	}
	rel, err := filepath.Rel(m.root, filepath.Clean(absPath))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	relSlash := filepath.ToSlash(rel)

	// Per Git semantics, if any ancestor directory is ignored, descendants are also ignored.
	for i := 0; i < len(relSlash); i++ {
		if relSlash[i] == '/' {
			if m.matchSingle(relSlash[:i], true) {
				return true
			}
		}
	}
	return m.matchSingle(relSlash, isDir)
}

func (m *gitignoreMatcher) matchSingle(relSlash string, isDir bool) bool {
	ignored := false
	for _, rule := range m.rules {
		if rule.matches(relSlash, isDir) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func (r gitignoreRule) matches(relSlash string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	subPath := relSlash
	if r.baseDir != "" {
		prefix := r.baseDir + "/"
		if !strings.HasPrefix(relSlash, prefix) {
			return false
		}
		subPath = relSlash[len(prefix):]
	}
	if r.anchored {
		return matchGlob(r.pattern, subPath)
	}
	base := subPath
	if idx := strings.LastIndex(subPath, "/"); idx >= 0 {
		base = subPath[idx+1:]
	}
	return matchGlob(r.pattern, base)
}
