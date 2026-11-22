package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	readDefaultLimit   = 2000
	readMaxLimit       = 2000
	readMaxLineRunes   = 2000
	readMaxBytes       = 50 * 1024
	globMaxResults     = 200
	grepMaxMatches     = 100
	grepMaxBytes       = 50 * 1024
	grepMaxFileSize    = 2 * 1024 * 1024
	grepMaxLineRunes   = 500
	maxDiffContentSize = 512 * 1024
)

var skippedDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	trashDirName:    true,
	".venv":         true,
	"venv":          true,
	"__pycache__":   true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".ruff_cache":   true,
	".tox":          true,
	".nox":          true,
	".gradle":       true,
	".next":         true,
	".nuxt":         true,
	".turbo":        true,
	".cache":        true,
}

// shouldSkipDir reports whether a subdirectory should be skipped during workspace walks
// (glob, grep, and bash file-change snapshots). Besides well-known dependency/cache directories,
// it also detects custom-named Python virtual environments via their pyvenv.cfg marker.
func shouldSkipDir(dirPath, name string) bool {
	if skippedDirs[name] {
		return true
	}
	if _, err := os.Stat(filepath.Join(dirPath, "pyvenv.cfg")); err == nil {
		return true
	}
	return false
}

func toolErrorResult(msg string, workdir string) (ToolExecuteResult, error) {
	meta := map[string]any{}
	if workdir != "" {
		meta["workdir"] = workdir
	}
	return ToolExecuteResult{
		ModelContent: msg,
		IsError:      true,
		UIMeta:       meta,
	}, nil
}

// executeRead implements read(file_path, offset?, limit?) with streaming UTF-8 line reading.
func executeRead(ctx context.Context, req ToolExecuteRequest) (ToolExecuteResult, error) {
	rawPath := stringInput(req.Input, "file_path")
	if rawPath == "" {
		return toolErrorResult("Error: file_path is required", req.Workdir)
	}

	allowOutside := allowOutsideForRequest(req)
	root, _, err := resolveRootForRequest(req, false)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), req.Workdir)
	}

	resolved, _, err := resolveAndValidatePath(rawPath, root, allowOutside, true)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), root)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("Error reading %s: %v", rawPath, err), root)
	}
	if info.IsDir() {
		return toolErrorResult(fmt.Sprintf("Error: %s is a directory, not a file", rawPath), root)
	}

	f, err := os.Open(resolved)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("Error opening %s: %v", rawPath, err), root)
	}
	defer f.Close()

	// Binary check on first 512 bytes.
	probe := make([]byte, 512)
	n, _ := f.Read(probe)
	if isBinaryData(probe[:n]) {
		return toolErrorResult(fmt.Sprintf("Error: %s appears to be a binary file and cannot be read as text", rawPath), root)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return toolErrorResult(fmt.Sprintf("Error reading %s: %v", rawPath, err), root)
	}

	offset := intInput(req.Input, "offset", 1)
	if offset < 1 {
		offset = 1
	}
	limit := intInput(req.Input, "limit", readDefaultLimit)
	if limit <= 0 {
		limit = readDefaultLimit
	}
	if limit > readMaxLimit {
		limit = readMaxLimit
	}

	reader := bufio.NewReaderSize(f, 32*1024)
	var out strings.Builder
	lineNum := 0
	linesEmitted := 0
	truncated := false
	truncateReason := ""

	for {
		if ctx.Err() != nil {
			return toolErrorResult("Error: operation cancelled", root)
		}
		line, rErr := reader.ReadString('\n')
		if len(line) == 0 && errors.Is(rErr, io.EOF) {
			break
		}
		lineNum++
		if lineNum < offset {
			if errors.Is(rErr, io.EOF) {
				break
			}
			continue
		}

		if linesEmitted >= limit {
			truncated = true
			truncateReason = fmt.Sprintf("line limit (%d) reached", limit)
			break
		}

		cleanLine := strings.TrimRight(line, "\r\n")
		cleanLine = truncateRunes(cleanLine, readMaxLineRunes)
		formatted := fmt.Sprintf("%6d\t%s\n", lineNum, cleanLine)

		if out.Len()+len(formatted) > readMaxBytes && linesEmitted > 0 {
			truncated = true
			truncateReason = fmt.Sprintf("byte limit (%d KB) reached", readMaxBytes/1024)
			break
		}

		out.WriteString(formatted)
		linesEmitted++

		if errors.Is(rErr, io.EOF) {
			break
		}
		if rErr != nil {
			return toolErrorResult(fmt.Sprintf("Error reading %s: %v", rawPath, rErr), root)
		}
	}

	if linesEmitted == 0 {
		if lineNum == 0 {
			out.WriteString("(empty file)")
		} else {
			out.WriteString(fmt.Sprintf("(offset %d is beyond end of file; total lines: %d)", offset, lineNum))
		}
	} else if truncated {
		nextOffset := offset + linesEmitted
		out.WriteString(fmt.Sprintf("\n[Truncated: %s. Use offset=%d to read more.]", truncateReason, nextOffset))
	}

	meta := map[string]any{
		"truncated": truncated,
	}
	if root != "" {
		meta["workdir"] = root
	}

	return ToolExecuteResult{
		ModelContent: strings.TrimRight(out.String(), "\n"),
		IsError:      false,
		UIMeta:       meta,
	}, nil
}

// executeWrite implements write(file_path, content) with atomic temp-file + rename and lazy scratch dir.
func executeWrite(_ context.Context, req ToolExecuteRequest) (ToolExecuteResult, error) {
	rawPath := stringInput(req.Input, "file_path")
	if rawPath == "" {
		return toolErrorResult("Error: file_path is required", req.Workdir)
	}
	contentVal, exists := req.Input["content"]
	if !exists || contentVal == nil {
		return toolErrorResult("Error: content is required", req.Workdir)
	}
	content, ok := contentVal.(string)
	if !ok {
		return toolErrorResult("Error: content must be a string", req.Workdir)
	}

	allowOutside := allowOutsideForRequest(req)

	// Pre-validate against candidate root BEFORE creating any scratch directory.
	candidateRoot, _, err := resolveRootForRequest(req, false)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), req.Workdir)
	}
	if _, _, err := resolveAndValidatePath(rawPath, candidateRoot, allowOutside, true); err != nil {
		return toolErrorResult("Error: "+err.Error(), candidateRoot)
	}

	// Now lazily create scratch directory if needed.
	root, assignedScratch, err := resolveRootForRequest(req, true)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), req.Workdir)
	}

	resolved, _, err := resolveAndValidatePath(rawPath, root, allowOutside, true)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), root)
	}

	// Capture previous content for UI diff if file already exists.
	var oldText string
	var filePerm os.FileMode = 0o644
	if info, statErr := os.Stat(resolved); statErr == nil {
		if info.IsDir() {
			return toolErrorResult(fmt.Sprintf("Error: %s is a directory", rawPath), root)
		}
		filePerm = info.Mode().Perm()
		if info.Size() <= maxDiffContentSize {
			if data, rErr := os.ReadFile(resolved); rErr == nil && !isBinaryData(data) {
				oldText = string(data)
			}
		}
	}

	if err := atomicWriteFile(resolved, []byte(content), filePerm); err != nil {
		return toolErrorResult(fmt.Sprintf("Error writing %s: %v", rawPath, err), root)
	}

	diffNew := content
	if len(diffNew) > maxDiffContentSize {
		diffNew = diffNew[:maxDiffContentSize]
	}

	meta := map[string]any{
		"producedFiles": []string{resolved},
		"diffs": []ToolFileDiff{
			{
				Path:    resolved,
				OldText: oldText,
				NewText: diffNew,
			},
		},
	}
	if root != "" {
		meta["workdir"] = root
	}

	return ToolExecuteResult{
		ModelContent:    fmt.Sprintf("Wrote %d bytes to %s", len(content), resolved),
		IsError:         false,
		AssignedScratch: assignedScratch,
		UIMeta:          meta,
	}, nil
}

// executeEdit implements edit(file_path, old_string, new_string, replace_all?) with exact literal matching.
func executeEdit(_ context.Context, req ToolExecuteRequest) (ToolExecuteResult, error) {
	rawPath := stringInput(req.Input, "file_path")
	if rawPath == "" {
		return toolErrorResult("Error: file_path is required", req.Workdir)
	}
	oldStr := stringInput(req.Input, "old_string")
	if oldStr == "" {
		return toolErrorResult("Error: old_string must not be empty", req.Workdir)
	}
	newStr := stringInput(req.Input, "new_string")
	if oldStr == newStr {
		return toolErrorResult("Error: old_string and new_string are identical", req.Workdir)
	}
	replaceAll := boolInput(req.Input, "replace_all")

	allowOutside := allowOutsideForRequest(req)

	root, _, err := resolveRootForRequest(req, false)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), req.Workdir)
	}

	resolved, _, err := resolveAndValidatePath(rawPath, root, allowOutside, true)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), root)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("Error: cannot edit %s: %v", rawPath, err), root)
	}
	if info.IsDir() {
		return toolErrorResult(fmt.Sprintf("Error: %s is a directory", rawPath), root)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("Error reading %s: %v", rawPath, err), root)
	}
	if isBinaryData(data) {
		return toolErrorResult(fmt.Sprintf("Error: %s is a binary file and cannot be edited", rawPath), root)
	}

	oldContent := string(data)
	count := strings.Count(oldContent, oldStr)
	if count == 0 {
		return toolErrorResult(fmt.Sprintf("Error: old_string not found in %s", rawPath), root)
	}
	if count > 1 && !replaceAll {
		return toolErrorResult(
			fmt.Sprintf("Error: old_string matched %d times in %s; provide more surrounding context or set replace_all=true", count, rawPath),
			root,
		)
	}

	var newContent string
	replaced := 1
	if replaceAll {
		newContent = strings.ReplaceAll(oldContent, oldStr, newStr)
		replaced = count
	} else {
		newContent = strings.Replace(oldContent, oldStr, newStr, 1)
	}

	if err := atomicWriteFile(resolved, []byte(newContent), info.Mode().Perm()); err != nil {
		return toolErrorResult(fmt.Sprintf("Error writing %s: %v", rawPath, err), root)
	}

	var assignedScratch string
	if strings.TrimSpace(req.Workdir) == "" && root != "" {
		if canonical, cErr := filepath.EvalSymlinks(root); cErr == nil {
			assignedScratch = canonical
		} else {
			assignedScratch = root
		}
	}

	diffOld := oldContent
	diffNew := newContent
	if len(diffOld) > maxDiffContentSize {
		diffOld = oldStr
		diffNew = newStr
	}

	meta := map[string]any{
		"producedFiles": []string{resolved},
		"diffs": []ToolFileDiff{
			{
				Path:    resolved,
				OldText: diffOld,
				NewText: diffNew,
			},
		},
	}
	if root != "" {
		meta["workdir"] = root
	}

	return ToolExecuteResult{
		ModelContent:    fmt.Sprintf("Replaced %d occurrence(s) in %s", replaced, resolved),
		IsError:         false,
		AssignedScratch: assignedScratch,
		UIMeta:          meta,
	}, nil
}

// executeDeleteFile implements delete_file(file_path) using os.Lstat and safe trash move.
func executeDeleteFile(_ context.Context, req ToolExecuteRequest) (ToolExecuteResult, error) {
	rawPath := stringInput(req.Input, "file_path")
	if rawPath == "" {
		return toolErrorResult("Error: file_path is required", req.Workdir)
	}

	allowOutside := allowOutsideForRequest(req)
	root, _, err := resolveRootForRequest(req, false)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), req.Workdir)
	}

	// followLeafSymlink = false: never follow a symlink target outside the workspace.
	resolved, _, err := resolveAndValidatePath(rawPath, root, allowOutside, false)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), root)
	}

	trashedPath, err := moveToTrash(resolved, root)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("Error deleting %s: %v", rawPath, err), root)
	}

	meta := map[string]any{
		"trashedPath": trashedPath,
	}
	if root != "" {
		meta["workdir"] = root
	}

	return ToolExecuteResult{
		ModelContent: fmt.Sprintf("Moved %s to trash (%s)", resolved, trashedPath),
		IsError:      false,
		UIMeta:       meta,
	}, nil
}

type globEntry struct {
	path    string
	modTime time.Time
}

// executeGlob implements glob(pattern, path?) using native WalkDir, sorted by newest mtime first.
func executeGlob(ctx context.Context, req ToolExecuteRequest) (ToolExecuteResult, error) {
	pattern := stringInput(req.Input, "pattern")
	if pattern == "" {
		return toolErrorResult("Error: pattern is required", req.Workdir)
	}

	allowOutside := allowOutsideForRequest(req)
	root, _, err := resolveRootForRequest(req, false)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), req.Workdir)
	}

	rawBase := stringInput(req.Input, "path")
	searchRoot := root
	if rawBase != "" {
		resolved, _, rErr := resolveAndValidatePath(rawBase, root, allowOutside, true)
		if rErr != nil {
			return toolErrorResult("Error: "+rErr.Error(), root)
		}
		searchRoot = resolved
	} else if searchRoot != "" {
		resolved, cErr := canonicalizePath(searchRoot)
		if cErr != nil {
			return toolErrorResult("Error: "+cErr.Error(), root)
		}
		searchRoot = resolved
	} else {
		return toolErrorResult("Error: path or active workspace is required", req.Workdir)
	}

	info, err := os.Stat(searchRoot)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("Error accessing %s: %v", searchRoot, err), root)
	}
	if !info.IsDir() {
		return toolErrorResult(fmt.Sprintf("Error: %s is not a directory", searchRoot), root)
	}

	normPattern := filepath.ToSlash(strings.TrimPrefix(pattern, "./"))
	var matches []globEntry

	gitignore := newGitignoreMatcher(searchRoot)
	if rawBase != "" && gitignore.isIgnored(searchRoot, true) {
		// When the caller explicitly targets an ignored directory, allow searching inside it.
		gitignore = nil
	}

	walkErr := filepath.WalkDir(searchRoot, func(path string, d fs.DirEntry, wErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if wErr != nil {
			return nil
		}
		if d.IsDir() {
			if path != searchRoot {
				if shouldSkipDir(path, d.Name()) || gitignore.isIgnored(path, true) {
					return filepath.SkipDir
				}
			}
			gitignore.loadDir(path)
			return nil
		}
		if gitignore.isIgnored(path, false) {
			return nil
		}
		rel, rErr := filepath.Rel(searchRoot, path)
		if rErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if matchGlob(normPattern, relSlash) || (!strings.Contains(normPattern, "/") && matchGlob(normPattern, d.Name())) {
			var modTime time.Time
			if fi, fErr := d.Info(); fErr == nil {
				modTime = fi.ModTime()
			}
			matches = append(matches, globEntry{path: relSlash, modTime: modTime})
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return toolErrorResult(fmt.Sprintf("Error searching %s: %v", searchRoot, walkErr), root)
	}
	if ctx.Err() != nil {
		return toolErrorResult("Error: operation cancelled", root)
	}

	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].modTime.After(matches[j].modTime)
		}
		return matches[i].path < matches[j].path
	})

	truncated := false
	total := len(matches)
	if len(matches) > globMaxResults {
		matches = matches[:globMaxResults]
		truncated = true
	}

	var out strings.Builder
	if len(matches) == 0 {
		out.WriteString("(no files matched)")
	} else {
		for _, m := range matches {
			out.WriteString(m.path)
			out.WriteByte('\n')
		}
		if truncated {
			out.WriteString(fmt.Sprintf("[Truncated: showing %d of %d matches.]", len(matches), total))
		}
	}

	meta := map[string]any{
		"truncated": truncated,
	}
	if root != "" {
		meta["workdir"] = root
	}

	return ToolExecuteResult{
		ModelContent: strings.TrimRight(out.String(), "\n"),
		IsError:      false,
		UIMeta:       meta,
	}, nil
}

// executeGrep implements grep(pattern, path?, include?) using native WalkDir + Go regexp.
func executeGrep(ctx context.Context, req ToolExecuteRequest) (ToolExecuteResult, error) {
	pattern := stringInput(req.Input, "pattern")
	if pattern == "" {
		return toolErrorResult("Error: pattern is required", req.Workdir)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("Error: invalid regular expression %q: %v", pattern, err), req.Workdir)
	}

	include := filepath.ToSlash(strings.TrimSpace(stringInput(req.Input, "include")))

	allowOutside := allowOutsideForRequest(req)
	root, _, err := resolveRootForRequest(req, false)
	if err != nil {
		return toolErrorResult("Error: "+err.Error(), req.Workdir)
	}

	rawBase := stringInput(req.Input, "path")
	searchTarget := root
	if rawBase != "" {
		resolved, _, rErr := resolveAndValidatePath(rawBase, root, allowOutside, true)
		if rErr != nil {
			return toolErrorResult("Error: "+rErr.Error(), root)
		}
		searchTarget = resolved
	} else if searchTarget != "" {
		resolved, cErr := canonicalizePath(searchTarget)
		if cErr != nil {
			return toolErrorResult("Error: "+cErr.Error(), root)
		}
		searchTarget = resolved
	} else {
		return toolErrorResult("Error: path or active workspace is required", req.Workdir)
	}

	info, err := os.Stat(searchTarget)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("Error accessing %s: %v", searchTarget, err), root)
	}

	var out strings.Builder
	totalMatches := 0
	truncated := false
	errLimitReached := errors.New("grep limit reached")

	scanFile := func(filePath, displayPath string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fi, sErr := os.Stat(filePath)
		if sErr != nil || fi.IsDir() || fi.Size() > grepMaxFileSize {
			return nil
		}
		f, oErr := os.Open(filePath)
		if oErr != nil {
			return nil
		}
		defer f.Close()

		probe := make([]byte, 512)
		n, _ := f.Read(probe)
		if isBinaryData(probe[:n]) {
			return nil
		}
		if _, sErr := f.Seek(0, io.SeekStart); sErr != nil {
			return nil
		}

		reader := bufio.NewReaderSize(f, 32*1024)
		lineNum := 0
		headerWritten := false
		for {
			line, rErr := reader.ReadString('\n')
			if len(line) == 0 && errors.Is(rErr, io.EOF) {
				break
			}
			lineNum++
			clean := strings.TrimRight(line, "\r\n")
			if re.MatchString(clean) {
				if totalMatches >= grepMaxMatches || out.Len() >= grepMaxBytes {
					truncated = true
					return errLimitReached
				}
				if !headerWritten {
					if out.Len() > 0 {
						out.WriteByte('\n')
					}
					out.WriteString(displayPath)
					out.WriteString(":\n")
					headerWritten = true
				}
				entry := fmt.Sprintf("  %d: %s\n", lineNum, truncateRunes(clean, grepMaxLineRunes))
				if out.Len()+len(entry) > grepMaxBytes && totalMatches > 0 {
					truncated = true
					return errLimitReached
				}
				out.WriteString(entry)
				totalMatches++
			}
			if errors.Is(rErr, io.EOF) {
				break
			}
			if rErr != nil {
				break
			}
		}
		return nil
	}

	if !info.IsDir() {
		display := filepath.Base(searchTarget)
		if root != "" {
			if canonicalRoot, cErr := canonicalizePath(root); cErr == nil {
				if rel, rErr := filepath.Rel(canonicalRoot, searchTarget); rErr == nil && !strings.HasPrefix(rel, "..") {
					display = filepath.ToSlash(rel)
				}
			}
		}
		_ = scanFile(searchTarget, display)
	} else {
		gitignore := newGitignoreMatcher(searchTarget)
		if rawBase != "" && gitignore.isIgnored(searchTarget, true) {
			// When the caller explicitly targets an ignored directory, allow searching inside it.
			gitignore = nil
		}
		walkErr := filepath.WalkDir(searchTarget, func(path string, d fs.DirEntry, wErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if wErr != nil {
				return nil
			}
			if d.IsDir() {
				if path != searchTarget {
					if shouldSkipDir(path, d.Name()) || gitignore.isIgnored(path, true) {
						return filepath.SkipDir
					}
				}
				gitignore.loadDir(path)
				return nil
			}
			// Skip symlinks during recursive walk.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if gitignore.isIgnored(path, false) {
				return nil
			}
			rel, rErr := filepath.Rel(searchTarget, path)
			if rErr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if include != "" {
				if !matchGlob(include, relSlash) && !matchGlob(include, d.Name()) {
					return nil
				}
			}
			return scanFile(path, relSlash)
		})
		if walkErr != nil && !errors.Is(walkErr, errLimitReached) && !errors.Is(walkErr, context.Canceled) {
			return toolErrorResult(fmt.Sprintf("Error searching %s: %v", searchTarget, walkErr), root)
		}
	}

	if ctx.Err() != nil {
		return toolErrorResult("Error: operation cancelled", root)
	}

	if totalMatches == 0 {
		out.WriteString("(no matches found)")
	} else if truncated {
		out.WriteString(fmt.Sprintf("\n[Truncated: stopped at %d matches / %d KB limit.]", totalMatches, grepMaxBytes/1024))
	}

	meta := map[string]any{
		"truncated": truncated,
	}
	if root != "" {
		meta["workdir"] = root
	}

	return ToolExecuteResult{
		ModelContent: strings.TrimRight(out.String(), "\n"),
		IsError:      false,
		UIMeta:       meta,
	}, nil
}

// atomicWriteFile writes data via a temporary file in the target directory and renames it atomically.
func atomicWriteFile(targetPath string, data []byte, perm os.FileMode) error {
	parent := filepath.Dir(targetPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(parent, ".vela-write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func isBinaryData(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return true
	}
	return false
}

func truncateRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	count := 0
	for idx := range s {
		if count == maxRunes {
			return s[:idx] + "...[truncated]"
		}
		count++
	}
	return s
}

// matchGlob matches slash-separated paths supporting ** recursive wildcards alongside filepath.Match syntax.
func matchGlob(pattern, name string) bool {
	patSegs := strings.Split(pattern, "/")
	nameSegs := strings.Split(name, "/")
	return matchGlobSegments(patSegs, nameSegs)
}

func matchGlobSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse consecutive ** segments.
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchGlobSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		matched, err := filepath.Match(pat[0], name[0])
		if err != nil || !matched {
			return false
		}
		pat = pat[1:]
		name = name[1:]
	}
	return len(name) == 0
}
