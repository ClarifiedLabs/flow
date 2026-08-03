package historyarchive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

func ValidatePath(name string, maxBytes int) error {
	if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || strings.Contains(name, "\\") {
		return fmt.Errorf("%w: invalid path %q", ErrUnsafePath, name)
	}
	if maxBytes > 0 && len(name) > maxBytes {
		return fmt.Errorf("%w: path exceeds %d bytes", ErrLimitExceeded, maxBytes)
	}
	if strings.HasPrefix(name, "/") || looksVolumeQualified(name) || path.Clean(name) != name {
		return fmt.Errorf("%w: non-relative path %q", ErrUnsafePath, name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: invalid component in %q", ErrUnsafePath, name)
		}
	}
	return nil
}

func ValidateLink(name, target string, maxBytes int) error {
	if err := ValidatePath(name, maxBytes); err != nil {
		return err
	}
	if target == "" || !utf8.ValidString(target) || strings.ContainsRune(target, 0) || strings.Contains(target, "\\") || strings.HasPrefix(target, "/") || looksVolumeQualified(target) {
		return fmt.Errorf("%w: invalid link %q -> %q", ErrUnsafePath, name, target)
	}
	if maxBytes > 0 && len(target) > maxBytes {
		return fmt.Errorf("%w: link target exceeds %d bytes", ErrLimitExceeded, maxBytes)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%w: escaping link %q -> %q", ErrUnsafePath, name, target)
	}
	return nil
}

func looksVolumeQualified(name string) bool {
	return len(name) >= 2 && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) && name[1] == ':'
}

func foldedPath(s string) string {
	return strings.Map(func(r rune) rune { return unicode.ToLower(r) }, s)
}

func validateFiles(files []File, limits Limits) error {
	if len(files) > limits.MaxEntries {
		return fmt.Errorf("%w: %d logical entries", ErrLimitExceeded, len(files))
	}
	seen := make(map[string]string, len(files))
	byPath := make(map[string]File, len(files))
	for i, file := range files {
		if err := ValidatePath(file.Path, limits.MaxPathBytes); err != nil {
			return fmt.Errorf("file %d: %w", i, err)
		}
		folded := foldedPath(file.Path)
		if prior, ok := seen[folded]; ok {
			return fmt.Errorf("%w: path collision %q and %q", ErrInvalidArchive, prior, file.Path)
		}
		seen[folded] = file.Path
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			if _, ok := byPath[parent]; ok {
				return fmt.Errorf("%w: file/directory conflict at %q", ErrInvalidArchive, parent)
			}
		}
		if file.Mode != 0600 && file.Mode != 0700 {
			return fmt.Errorf("%w: non-normalized mode for %q", ErrInvalidArchive, file.Path)
		}
		switch file.Type {
		case FileRegular:
			if file.Size < 0 || file.Size > limits.MaxFileBytes || !validDigest(file.SHA256) || file.Blob != blobName(file.SHA256) || file.LinkTarget != "" {
				return fmt.Errorf("%w: invalid regular file %q", ErrInvalidArchive, file.Path)
			}
		case FileSymlink:
			if file.Size != 0 || file.SHA256 != "" || file.Blob != "" || file.Mode != 0600 {
				return fmt.Errorf("%w: invalid symlink metadata %q", ErrInvalidArchive, file.Path)
			}
			if err := ValidateLink(file.Path, file.LinkTarget, limits.MaxPathBytes); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: file type %q", ErrInvalidArchive, file.Type)
		}
		byPath[file.Path] = file
	}
	// A link chain must terminate without cycling. Missing targets are allowed:
	// Harness may intentionally retain a dangling but root-confined link.
	for _, file := range files {
		if file.Type != FileSymlink {
			continue
		}
		visited := map[string]bool{file.Path: true}
		current := path.Clean(path.Join(path.Dir(file.Path), file.LinkTarget))
		for {
			next, ok := byPath[current]
			if !ok || next.Type != FileSymlink {
				break
			}
			if visited[current] {
				return fmt.Errorf("%w: cyclic link at %q", ErrUnsafePath, file.Path)
			}
			visited[current] = true
			current = path.Clean(path.Join(path.Dir(current), next.LinkTarget))
			if current == ".." || strings.HasPrefix(current, "../") {
				return fmt.Errorf("%w: link chain escapes", ErrUnsafePath)
			}
		}
	}
	return nil
}

func validDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func blobName(digest string) string { return "blobs/" + digest }

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func isGitMetadataPath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}

func deniedFlowPath(name string) bool {
	return name == ".flow/session" || strings.HasPrefix(name, ".flow/session/") || name == ".flow/attachments" || strings.HasPrefix(name, ".flow/attachments/") || name == ".flow/history" || strings.HasPrefix(name, ".flow/history/")
}
