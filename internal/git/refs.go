package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
)

type TaskBranchRef struct {
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
	FileCount int            `json:"-"`
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

// UpdateRef points ref at sha in the bare exchange repository directly. It
// bypasses receive hooks, so it is reserved for coordinator-local
// administrative writes that precede any other writer (seeding a feature
// branch at creation).
func UpdateRef(ctx context.Context, exchangeRepoPath, ref, sha string) error {
	ref = strings.TrimSpace(ref)
	sha = strings.TrimSpace(sha)
	if ref == "" || sha == "" {
		return errors.New("ref and sha are required")
	}
	if err := gitBareRun(ctx, exchangeRepoPath, nil, "update-ref", ref, sha); err != nil {
		return fmt.Errorf("update %s to %s: %w", ref, sha, err)
	}

	return nil
}

// CreateOrVerifyRef creates a coordinator-owned ref exactly once. A replay is
// successful only when the existing ref already names expectedSHA; it can
// never repoint a ref created by an earlier attempt.
func CreateOrVerifyRef(ctx context.Context, exchangeRepoPath, ref, expectedSHA string) error {
	_, err := CreateOrVerifyRefOwned(ctx, exchangeRepoPath, ref, expectedSHA)
	return err
}

// CreateOrVerifyRefOwned has CreateOrVerifyRef's compare-and-swap semantics and
// reports whether this call created the previously absent ref. An existing ref
// at expectedSHA is a successful replay but is not attributed to this caller.
func CreateOrVerifyRefOwned(ctx context.Context, exchangeRepoPath, ref, expectedSHA string) (bool, error) {
	ref = strings.TrimSpace(ref)
	expectedSHA = strings.TrimSpace(expectedSHA)
	if ref == "" || expectedSHA == "" {
		return false, errors.New("ref and expected sha are required")
	}
	if (len(expectedSHA) != 40 && len(expectedSHA) != 64) || !isHexObjectID(expectedSHA) {
		return false, errors.New("expected sha must be a full SHA-1 or SHA-256 object id")
	}
	if err := gitBareRun(ctx, exchangeRepoPath, nil, "check-ref-format", ref); err != nil {
		return false, fmt.Errorf("validate ref %s: %w", ref, err)
	}
	if exists, err := CommitExists(ctx, exchangeRepoPath, expectedSHA); err != nil {
		return false, err
	} else if !exists {
		return false, fmt.Errorf("expected commit %s does not exist", expectedSHA)
	}

	if symbolic, err := gitExitCode(ctx, "", exchangeRepoPath, nil, "symbolic-ref", "-q", ref); err != nil {
		return false, fmt.Errorf("inspect ref %s: %w", ref, err)
	} else if symbolic == 0 {
		return false, fmt.Errorf("ref %s is symbolic; immutable refs must be direct", ref)
	}

	zeroSHA := strings.Repeat("0", len(expectedSHA))
	createErr := gitBareRun(ctx, exchangeRepoPath, nil, "update-ref", "--no-deref", ref, expectedSHA, zeroSHA)
	if createErr == nil {
		return true, nil
	}
	if symbolic, err := gitExitCode(ctx, "", exchangeRepoPath, nil, "symbolic-ref", "-q", ref); err != nil {
		return false, fmt.Errorf("inspect ref %s after create conflict: %w", ref, err)
	} else if symbolic == 0 {
		return false, fmt.Errorf("ref %s is symbolic; immutable refs must be direct", ref)
	}

	actualSHA, resolveErr := gitBareOutput(ctx, exchangeRepoPath, nil, "rev-parse", "--verify", ref)
	if resolveErr == nil && strings.TrimSpace(actualSHA) == expectedSHA {
		return false, nil
	}
	if resolveErr != nil {
		return false, fmt.Errorf("create ref %s at %s: %w", ref, expectedSHA, createErr)
	}
	return false, fmt.Errorf("ref %s already points to %s, expected %s", ref, strings.TrimSpace(actualSHA), expectedSHA)
}

// DeleteRefIfMatches removes a direct ref only while it still names expectedSHA.
// Git's old-value check prevents cleanup from deleting a concurrently moved ref.
func DeleteRefIfMatches(ctx context.Context, exchangeRepoPath, ref, expectedSHA string) error {
	ref = strings.TrimSpace(ref)
	expectedSHA = strings.TrimSpace(expectedSHA)
	if ref == "" || expectedSHA == "" {
		return errors.New("ref and expected sha are required")
	}
	if (len(expectedSHA) != 40 && len(expectedSHA) != 64) || !isHexObjectID(expectedSHA) {
		return errors.New("expected sha must be a full SHA-1 or SHA-256 object id")
	}
	if err := gitBareRun(ctx, exchangeRepoPath, nil, "update-ref", "--no-deref", "-d", ref, expectedSHA); err != nil {
		return fmt.Errorf("delete %s at %s: %w", ref, expectedSHA, err)
	}
	return nil
}

func isHexObjectID(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

// CommitExists reports whether commitish resolves to a commit in the bare
// exchange repository. Missing objects are not errors.
func CommitExists(ctx context.Context, exchangeRepoPath, commitish string) (bool, error) {
	commitish = strings.TrimSpace(commitish)
	if commitish == "" {
		return false, errors.New("commit is required")
	}
	if strings.HasPrefix(commitish, "-") {
		return false, errors.New("commit may not start with '-'")
	}
	result, err := runGit(ctx, "", exchangeRepoPath, nil, "cat-file", "-t", commitish)
	if err != nil {
		var commandErr *gitCommandError
		if errors.As(err, &commandErr) && (strings.Contains(commandErr.stderr, "Not a valid object name") ||
			strings.Contains(commandErr.stderr, "could not get object info")) {
			return false, nil
		}
		return false, fmt.Errorf("verify commit %s: %w", commitish, err)
	}
	return strings.TrimSpace(result.stdout) == "commit", nil
}

// MergeBase resolves the exact common ancestor used to describe the source
// change relative to its target base.
func MergeBase(ctx context.Context, exchangeRepoPath, left, right string) (string, error) {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return "", errors.New("left and right revisions are required")
	}
	sha, err := gitBareOutput(ctx, exchangeRepoPath, nil, "merge-base", left, right)
	if err != nil {
		return "", fmt.Errorf("resolve merge base for %s and %s: %w", left, right, err)
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", fmt.Errorf("resolve merge base for %s and %s: empty result", left, right)
	}
	return sha, nil
}

// CanonicalDiffDigest hashes a bounded-independent, deterministic description
// of the merge-base-to-head change. The detailed patch stays in Git; SQLite
// stores this fingerprint plus bounded file statistics.
func CanonicalDiffDigest(ctx context.Context, exchangeRepoPath, mergeBaseSHA, headSHA string) (string, error) {
	mergeBaseSHA = strings.TrimSpace(mergeBaseSHA)
	headSHA = strings.TrimSpace(headSHA)
	if mergeBaseSHA == "" || headSHA == "" {
		return "", errors.New("merge base and head sha are required")
	}
	diffLabel := mergeBaseSHA + ".." + headSHA
	gitEnv := []string{"LC_ALL=C", "LANG=C"}
	digest := sha256.New()
	if _, err := io.WriteString(digest, "flow-canonical-diff-v1\x00"); err != nil {
		return "", err
	}
	if err := streamCanonicalDiffPart(ctx, exchangeRepoPath, gitEnv, digest,
		"--numstat", "-z", "--no-renames", mergeBaseSHA, headSHA, "--", ".", ":(exclude).flow/session"); err != nil {
		return "", fmt.Errorf("read canonical stats %s: %w", diffLabel, err)
	}
	if _, err := digest.Write([]byte{0}); err != nil {
		return "", err
	}
	if err := streamCanonicalDiffPart(ctx, exchangeRepoPath, gitEnv, digest,
		"--no-ext-diff", "--no-textconv", "--binary", "--full-index", "--no-renames",
		mergeBaseSHA, headSHA, "--", ".", ":(exclude).flow/session"); err != nil {
		return "", fmt.Errorf("read canonical patch %s: %w", diffLabel, err)
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}

// RefDivergence reports how many commits branch is ahead of and behind base
// in the bare exchange repository. Both refs must exist.
func streamCanonicalDiffPart(ctx context.Context, exchangeRepoPath string, gitEnv []string, digest hash.Hash, args ...string) error {
	gitArgs := []string{"-c", "core.quotePath=true", "-c", "color.ui=false", "diff", "--diff-algorithm=myers"}
	gitArgs = append(gitArgs, args...)
	return writeGitOutput(ctx, "", exchangeRepoPath, gitEnv, digest, gitArgs...)
}

func RefDivergence(ctx context.Context, exchangeRepoPath, base, branch string) (int, int, error) {
	base = strings.TrimSpace(base)
	branch = strings.TrimSpace(branch)
	if base == "" || branch == "" {
		return 0, 0, errors.New("base and branch are required")
	}
	ahead, err := revListCount(ctx, exchangeRepoPath, "refs/heads/"+base+"..refs/heads/"+branch)
	if err != nil {
		return 0, 0, err
	}
	behind, err := revListCount(ctx, exchangeRepoPath, "refs/heads/"+branch+"..refs/heads/"+base)
	if err != nil {
		return 0, 0, err
	}

	return ahead, behind, nil
}

func revListCount(ctx context.Context, exchangeRepoPath, rangeSpec string) (int, error) {
	output, err := gitBareOutput(ctx, exchangeRepoPath, nil, "rev-list", "--count", rangeSpec)
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", rangeSpec, err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, fmt.Errorf("parse rev-list count %q: %w", output, err)
	}

	return count, nil
}

func ListTaskBranchRefs(ctx context.Context, exchangeRepoPath string) ([]TaskBranchRef, error) {
	output, err := gitBareOutput(ctx, exchangeRepoPath, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/task")
	if err != nil {
		return nil, fmt.Errorf("list task branch refs: %w", err)
	}
	if strings.TrimSpace(output) == "" {
		return nil, nil
	}

	var refs []TaskBranchRef
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid task ref line %q", line)
		}
		ref := fields[0]
		if !taskRefPattern.MatchString(ref) {
			continue
		}
		refs = append(refs, TaskBranchRef{
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

// ChangedFileStats reports the changes introduced by newRef since its merge
// base with oldRef. Changes made only on oldRef after the refs diverged are not
// part of the change under review and must not appear as reverse edits.
func ChangedFileStats(ctx context.Context, exchangeRepoPath string, oldRef string, newRef string) (DiffStats, error) {
	oldRef, newRef, diffSpec, err := mergeBaseDiffSpec(oldRef, newRef)
	if err != nil {
		return DiffStats{}, err
	}

	return changedFileStats(ctx, exchangeRepoPath, oldRef, newRef, diffSpec, 0)
}

// ChangedFileStatsBounded reports exact aggregate counts while retaining at
// most maxFiles deterministic path details. This keeps evidence collection
// bounded even when Git emits numstat output for an enormous change.
func ChangedFileStatsBounded(ctx context.Context, exchangeRepoPath string, oldRef string, newRef string, maxFiles int) (DiffStats, error) {
	if maxFiles <= 0 {
		return DiffStats{}, errors.New("maximum retained changed files must be positive")
	}
	oldRef, newRef, diffSpec, err := mergeBaseDiffSpec(oldRef, newRef)
	if err != nil {
		return DiffStats{}, err
	}

	return changedFileStats(ctx, exchangeRepoPath, oldRef, newRef, diffSpec, maxFiles)
}

func changedFileStats(ctx context.Context, exchangeRepoPath string, oldRef string, newRef string, diffSpec string, maxFiles int) (DiffStats, error) {
	var stats DiffStats
	err := scanGitOutput(ctx, "", exchangeRepoPath, nil, func(line string) error {
		if line == "" {
			return nil
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			return fmt.Errorf("invalid diff numstat line %q", line)
		}
		file := DiffFileStat{Path: normalizeDiffPath(parts[2])}
		if excludedMergePath(file.Path) {
			return nil
		}
		if parts[0] == "-" || parts[1] == "-" {
			file.Binary = true
		} else {
			additions, err := strconv.Atoi(parts[0])
			if err != nil {
				return fmt.Errorf("parse additions in %q: %w", line, err)
			}
			deletions, err := strconv.Atoi(parts[1])
			if err != nil {
				return fmt.Errorf("parse deletions in %q: %w", line, err)
			}
			file.Additions = additions
			file.Deletions = deletions
		}
		stats.FileCount++
		stats.Additions += file.Additions
		stats.Deletions += file.Deletions
		stats.Files = retainChangedFile(stats.Files, file, maxFiles)
		return nil
	}, "diff", "--numstat", "--no-renames", diffSpec)
	if err != nil {
		return DiffStats{}, fmt.Errorf("list changed file stats %s...%s: %w", oldRef, newRef, err)
	}
	sort.Slice(stats.Files, func(i, j int) bool { return stats.Files[i].Path < stats.Files[j].Path })
	return stats, nil
}

func retainChangedFile(files []DiffFileStat, file DiffFileStat, maxFiles int) []DiffFileStat {
	if maxFiles <= 0 || len(files) < maxFiles {
		return append(files, file)
	}
	largest := 0
	for index := 1; index < len(files); index++ {
		if files[index].Path > files[largest].Path {
			largest = index
		}
	}
	if file.Path < files[largest].Path {
		files[largest] = file
	}
	return files
}

// ChangedFileDiff returns merge-base-relative file stats and parsed hunks for
// the changes introduced by newRef.
func ChangedFileDiff(ctx context.Context, exchangeRepoPath string, oldRef string, newRef string) (DiffStats, error) {
	oldRef, newRef, diffSpec, err := mergeBaseDiffSpec(oldRef, newRef)
	if err != nil {
		return DiffStats{}, err
	}
	stats, err := changedFileStats(ctx, exchangeRepoPath, oldRef, newRef, diffSpec, 0)
	if err != nil {
		return DiffStats{}, err
	}
	if len(stats.Files) == 0 {
		return stats, nil
	}
	result, err := runGit(ctx, "", exchangeRepoPath, nil, "diff", "--unified=3", "--no-renames", diffSpec)
	if err != nil {
		return DiffStats{}, fmt.Errorf("parse changed file hunks %s...%s: %w", oldRef, newRef, err)
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

func mergeBaseDiffSpec(oldRef string, newRef string) (string, string, string, error) {
	oldRef = strings.TrimSpace(oldRef)
	newRef = strings.TrimSpace(newRef)
	if oldRef == "" || newRef == "" {
		return "", "", "", fmt.Errorf("old and new refs are required")
	}

	return oldRef, newRef, oldRef + "..." + newRef, nil
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
