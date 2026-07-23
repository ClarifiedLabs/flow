package git

import "strings"

// Default identity for coordinator-created merge commits. Used when no commit
// identity is configured.
const (
	DefaultMergeCommitName  = "Flow Coordinator"
	DefaultMergeCommitEmail = "flow@example.invalid"
)

// CommitIdentity is a git author/committer identity. The zero value means
// "unconfigured"; callers apply their own default or inherit behavior.
type CommitIdentity struct {
	Name  string
	Email string
}

// Configured reports whether at least one field is set.
func (i CommitIdentity) Configured() bool {
	return strings.TrimSpace(i.Name) != "" || strings.TrimSpace(i.Email) != ""
}

// WithDefaults returns the identity with empty fields filled from fallback.
func (i CommitIdentity) WithDefaults(fallback CommitIdentity) CommitIdentity {
	result := i
	if strings.TrimSpace(result.Name) == "" {
		result.Name = fallback.Name
	}
	if strings.TrimSpace(result.Email) == "" {
		result.Email = fallback.Email
	}
	return result
}

// Env renders GIT_AUTHOR_*/GIT_COMMITTER_* assignments. These take precedence
// over git config and propagate to subprocesses (e.g. the harness and the
// shells it spawns).
func (i CommitIdentity) Env() []string {
	name := strings.TrimSpace(i.Name)
	email := strings.TrimSpace(i.Email)
	if name == "" && email == "" {
		return nil
	}
	return []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
	}
}
