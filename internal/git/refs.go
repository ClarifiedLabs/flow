package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type IssueBranchRef struct {
	Ref    string
	Branch string
	SHA    string
}

type TextFileAtRef struct {
	Path    string
	Content string
}

type DiffFileStat struct {
	Path      string     `json:"path"`
	Additions int        `json:"additions"`
	Deletions int        `json:"deletions"`
	Binary    bool       `json:"binary"`
	Hunks     []DiffHunk `json:"hunks,omitempty"`
}

type DiffHunk struct {
	OldStart int        `json:"old_start"`
	OldLines int        `json:"old_lines"`
	NewStart int        `json:"new_start"`
	NewLines int        `json:"new_lines"`
	Header   string     `json:"header"`
	Lines    []DiffLine `json:"lines,omitempty"`
}

type DiffLine struct {
	Kind    string `json:"kind"`
	OldLine *int   `json:"old_line,omitempty"`
	NewLine *int   `json:"new_line,omitempty"`
	Text    string `json:"text"`
}

type DiffStats struct {
	Files     []DiffFileStat `json:"files"`
	Additions int            `json:"additions"`
	Deletions int            `json:"deletions"`
}

// BranchTip resolves the current tip SHA of a branch in the bare exchange
// repository. Returns ok=false when the branch does not exist.
func BranchTip(ctx context.Context, exchangeRepoPath string, branch string) (string, bool, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", false, fmt.Errorf("branch is required")
	}
	exitCode, err := gitExitCode(ctx, "", exchangeRepoPath, nil, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return "", false, fmt.Errorf("check branch %s: %w", branch, err)
	}
	if exitCode != 0 {
		return "", false, nil
	}
	sha, err := gitBareOutput(ctx, exchangeRepoPath, nil, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return "", false, fmt.Errorf("resolve branch %s tip: %w", branch, err)
	}

	return strings.TrimSpace(sha), true, nil
}

func ListIssueBranchRefs(ctx context.Context, exchangeRepoPath string) ([]IssueBranchRef, error) {
	output, err := gitBareOutput(ctx, exchangeRepoPath, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/issue")
	if err != nil {
		return nil, fmt.Errorf("list issue branch refs: %w", err)
	}
	if strings.TrimSpace(output) == "" {
		return nil, nil
	}

	var refs []IssueBranchRef
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid issue ref line %q", line)
		}
		ref := fields[0]
		if !issueRefPattern.MatchString(ref) {
			continue
		}
		refs = append(refs, IssueBranchRef{
			Ref:    ref,
			Branch: strings.TrimPrefix(ref, "refs/heads/"),
			SHA:    fields[1],
		})
	}

	return refs, nil
}

func ReadTextFileAtRef(ctx context.Context, exchangeRepoPath string, ref string, path string) (string, bool, error) {
	ref = strings.TrimSpace(ref)
	path = strings.TrimSpace(path)
	if ref == "" {
		return "", false, fmt.Errorf("ref is required")
	}
	if path == "" {
		return "", false, fmt.Errorf("path is required")
	}
	if !treePathExists(ctx, exchangeRepoPath, ref, path) {
		return "", false, nil
	}

	result, err := runGit(ctx, "", exchangeRepoPath, nil, "show", ref+":"+path)
	if err != nil {
		return "", false, fmt.Errorf("read %s at %s: %w", path, ref, err)
	}

	return result.stdout, true, nil
}

func ListTextFilesAtRef(ctx context.Context, exchangeRepoPath string, ref string, prefix string) ([]TextFileAtRef, error) {
	ref = strings.TrimSpace(ref)
	prefix = strings.TrimSpace(prefix)
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	if prefix == "" {
		return nil, fmt.Errorf("prefix is required")
	}
	output, err := gitBareOutput(ctx, exchangeRepoPath, nil, "ls-tree", "-r", "--name-only", ref, "--", prefix)
	if err != nil {
		return nil, fmt.Errorf("list text files at %s: %w", ref, err)
	}
	if strings.TrimSpace(output) == "" {
		return nil, nil
	}

	var files []TextFileAtRef
	for _, path := range strings.Split(output, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		content, present, err := ReadTextFileAtRef(ctx, exchangeRepoPath, ref, path)
		if err != nil {
			return nil, err
		}
		if present {
			files = append(files, TextFileAtRef{Path: path, Content: content})
		}
	}

	return files, nil
}

func ChangedPaths(ctx context.Context, exchangeRepoPath string, oldRef string, newRef string) ([]string, error) {
	oldRef = strings.TrimSpace(oldRef)
	newRef = strings.TrimSpace(newRef)
	if oldRef == "" || newRef == "" {
		return nil, fmt.Errorf("old and new refs are required")
	}
	output, err := gitBareOutput(ctx, exchangeRepoPath, nil, "diff", "--name-only", oldRef, newRef)
	if err != nil {
		return nil, fmt.Errorf("list changed paths %s..%s: %w", oldRef, newRef, err)
	}
	if strings.TrimSpace(output) == "" {
		return nil, nil
	}
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		path := strings.TrimSpace(line)
		if path != "" {
			paths = append(paths, path)
		}
	}

	return paths, nil
}

func ChangedFileStats(ctx context.Context, exchangeRepoPath string, oldRef string, newRef string) (DiffStats, error) {
	oldRef = strings.TrimSpace(oldRef)
	newRef = strings.TrimSpace(newRef)
	if oldRef == "" || newRef == "" {
		return DiffStats{}, fmt.Errorf("old and new refs are required")
	}
	output, err := gitBareOutput(ctx, exchangeRepoPath, nil, "diff", "--numstat", "--no-renames", oldRef, newRef)
	if err != nil {
		return DiffStats{}, fmt.Errorf("list changed file stats %s..%s: %w", oldRef, newRef, err)
	}
	if strings.TrimSpace(output) == "" {
		return DiffStats{}, nil
	}

	var stats DiffStats
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			return DiffStats{}, fmt.Errorf("invalid diff numstat line %q", line)
		}
		file := DiffFileStat{Path: normalizeDiffPath(parts[2])}
		if excludedMergePath(file.Path) {
			continue
		}
		if parts[0] == "-" || parts[1] == "-" {
			file.Binary = true
		} else {
			if _, err := fmt.Sscan(parts[0], &file.Additions); err != nil {
				return DiffStats{}, fmt.Errorf("parse additions in %q: %w", line, err)
			}
			if _, err := fmt.Sscan(parts[1], &file.Deletions); err != nil {
				return DiffStats{}, fmt.Errorf("parse deletions in %q: %w", line, err)
			}
		}
		stats.Additions += file.Additions
		stats.Deletions += file.Deletions
		stats.Files = append(stats.Files, file)
	}

	return stats, nil
}

func ChangedFileDiff(ctx context.Context, exchangeRepoPath string, oldRef string, newRef string) (DiffStats, error) {
	stats, err := ChangedFileStats(ctx, exchangeRepoPath, oldRef, newRef)
	if err != nil {
		return DiffStats{}, err
	}
	if len(stats.Files) == 0 {
		return stats, nil
	}
	result, err := runGit(ctx, "", exchangeRepoPath, nil, "diff", "--unified=3", "--no-renames", oldRef, newRef)
	if err != nil {
		return DiffStats{}, fmt.Errorf("parse changed file hunks %s..%s: %w", oldRef, newRef, err)
	}
	output := strings.TrimSuffix(result.stdout, "\n")
	hunksByPath, err := parseDiffHunks(output)
	if err != nil {
		return DiffStats{}, err
	}
	for index, file := range stats.Files {
		stats.Files[index].Hunks = hunksByPath[file.Path]
	}

	return stats, nil
}

func parseDiffHunks(output string) (map[string][]DiffHunk, error) {
	hunksByPath := map[string][]DiffHunk{}
	var oldPath string
	var currentPath string
	var currentHunk *DiffHunk
	var oldLine int
	var newLine int
	flushHunk := func() {
		if currentPath == "" || currentHunk == nil {
			return
		}
		hunksByPath[currentPath] = append(hunksByPath[currentPath], *currentHunk)
		currentHunk = nil
	}

	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushHunk()
			oldPath = ""
			currentPath = ""
		case strings.HasPrefix(line, "@@ "):
			flushHunk()
			if currentPath == "" {
				continue
			}
			hunk, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			oldLine = hunk.OldStart
			newLine = hunk.NewStart
			currentHunk = &hunk
		case currentHunk != nil:
			diffLine, nextOldLine, nextNewLine := parseDiffLine(line, oldLine, newLine)
			currentHunk.Lines = append(currentHunk.Lines, diffLine)
			oldLine = nextOldLine
			newLine = nextNewLine
		case strings.HasPrefix(line, "--- "):
			flushHunk()
			oldPath = patchPath(strings.TrimSpace(strings.TrimPrefix(line, "--- ")))
		case strings.HasPrefix(line, "+++ "):
			flushHunk()
			newPath := patchPath(strings.TrimSpace(strings.TrimPrefix(line, "+++ ")))
			currentPath = newPath
			if currentPath == "" {
				currentPath = oldPath
			}
			if excludedMergePath(currentPath) {
				currentPath = ""
			}
		}
	}
	flushHunk()

	return hunksByPath, nil
}

func parseHunkHeader(header string) (DiffHunk, error) {
	parts := strings.Fields(header)
	if len(parts) < 3 || parts[0] != "@@" {
		return DiffHunk{}, fmt.Errorf("invalid diff hunk header %q", header)
	}
	oldStart, oldLines, err := parseHunkRange(parts[1], '-')
	if err != nil {
		return DiffHunk{}, fmt.Errorf("parse old range in %q: %w", header, err)
	}
	newStart, newLines, err := parseHunkRange(parts[2], '+')
	if err != nil {
		return DiffHunk{}, fmt.Errorf("parse new range in %q: %w", header, err)
	}

	return DiffHunk{
		OldStart: oldStart,
		OldLines: oldLines,
		NewStart: newStart,
		NewLines: newLines,
		Header:   header,
	}, nil
}

func parseHunkRange(value string, prefix byte) (int, int, error) {
	value = strings.TrimSpace(value)
	if value == "" || value[0] != prefix {
		return 0, 0, fmt.Errorf("range must start with %q", prefix)
	}
	value = value[1:]
	parts := strings.SplitN(value, ",", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	lines := 1
	if len(parts) == 2 {
		lines, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, err
		}
	}

	return start, lines, nil
}

func parseDiffLine(line string, oldLine int, newLine int) (DiffLine, int, int) {
	if line == "" {
		return DiffLine{Kind: "context", OldLine: intPointer(oldLine), NewLine: intPointer(newLine), Text: ""}, oldLine + 1, newLine + 1
	}
	switch line[0] {
	case '+':
		return DiffLine{Kind: "add", NewLine: intPointer(newLine), Text: line[1:]}, oldLine, newLine + 1
	case '-':
		return DiffLine{Kind: "delete", OldLine: intPointer(oldLine), Text: line[1:]}, oldLine + 1, newLine
	case '\\':
		return DiffLine{Kind: "meta", Text: line}, oldLine, newLine
	default:
		text := line
		if strings.HasPrefix(line, " ") {
			text = line[1:]
		}
		return DiffLine{Kind: "context", OldLine: intPointer(oldLine), NewLine: intPointer(newLine), Text: text}, oldLine + 1, newLine + 1
	}
}

func patchPath(path string) string {
	if path == "/dev/null" {
		return ""
	}
	path = unquoteGitPath(strings.TrimSpace(path))
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		return path[2:]
	}

	return path
}

func intPointer(value int) *int {
	return &value
}

func normalizeDiffPath(path string) string {
	path = unquoteGitPath(strings.TrimSpace(path))
	if strings.HasPrefix(path, "{") && strings.Contains(path, " => ") && strings.Contains(path, "}") {
		start := strings.LastIndex(path, " => ")
		end := strings.LastIndex(path, "}")
		if start >= 0 && end > start {
			return strings.TrimSpace(path[start+4 : end])
		}
	}
	if strings.Contains(path, " => ") {
		parts := strings.Split(path, " => ")
		return strings.TrimSpace(parts[len(parts)-1])
	}

	return path
}

func unquoteGitPath(path string) string {
	if len(path) >= 2 && path[0] == '"' {
		if unquoted, err := strconv.Unquote(path); err == nil {
			return unquoted
		}
	}

	return path
}

func excludedMergePath(path string) bool {
	path = strings.Trim(strings.TrimSpace(path), "/")
	return path == ".flow/session" ||
		strings.HasPrefix(path, ".flow/session/")
}

var excludedMergePathRoots = []string{".flow/session"}
