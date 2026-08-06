package checkverdict

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	VerdictFileName    = ".flow-verdict.json"
	CompletionFileName = ".flow-completion.json"
	CompletionProtocol = "flow_complete"
	SealVersion        = 1
)

type Mode string

const (
	ModeReview            Mode = "review"
	ModeReviewDiscovery   Mode = "review_discovery"
	ModeReviewAggregation Mode = "review_aggregation"
	ModeVerify            Mode = "verify"
)

func ParseMode(value string) (Mode, error) {
	mode := Mode(strings.TrimSpace(value))
	switch mode {
	case ModeReview, ModeReviewDiscovery, ModeReviewAggregation, ModeVerify:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid check completion mode %q", value)
	}
}

// VerdictReport is the structured outcome a reviewer or verifier writes.
type VerdictReport struct {
	Verdict         string                 `json:"verdict"`
	Reason          string                 `json:"reason"`
	Comments        []ReviewCommentReport  `json:"comments,omitempty"`
	Threads         []ThreadDecisionReport `json:"threads,omitempty"`
	DecisionRequest *ReviewDecisionRequest `json:"decision_request,omitempty"`
}

type ReviewCommentReport struct {
	SHA                string                  `json:"sha"`
	File               string                  `json:"file"`
	Line               int                     `json:"line"`
	Body               string                  `json:"body"`
	Severity           string                  `json:"severity"`
	IntroducedByChange *bool                   `json:"introduced_by_change"`
	Requirement        string                  `json:"requirement"`
	RequirementSource  string                  `json:"requirement_source,omitempty"`
	FindingBasis       string                  `json:"finding_basis,omitempty"`
	RemediationScope   string                  `json:"remediation_scope,omitempty"`
	ScopeRationale     string                  `json:"scope_rationale,omitempty"`
	DuplicateOf        string                  `json:"duplicate_of,omitempty"`
	FollowUp           string                  `json:"follow_up,omitempty"`
	TaskAction         *ReviewTaskActionReport `json:"task_action,omitempty"`
}

type ReviewDecisionRequest struct {
	Key            string `json:"key"`
	Question       string `json:"question"`
	Rationale      string `json:"rationale"`
	CommentIndexes []int  `json:"comment_indexes"`
}

type ReviewTaskActionReport struct {
	Action string `json:"action"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

func (r ReviewCommentReport) BlocksApproval() bool {
	if r.IntroducedByChange == nil || !*r.IntroducedByChange || r.DuplicateOf != "" {
		return false
	}
	return r.Severity == "critical" || r.Severity == "high"
}

type ThreadDecisionReport struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Body     string `json:"body"`
}

type ValidatedVerdict struct {
	Report VerdictReport
	Bytes  []byte
	Digest string
}

type Seal struct {
	Version       int    `json:"version"`
	JobID         string `json:"job_id"`
	CheckName     string `json:"check_name"`
	Mode          Mode   `json:"mode"`
	VerdictSHA256 string `json:"verdict_sha256"`
}

type Context struct {
	JobID     string
	CheckName string
	Mode      Mode
}

const (
	verdictReasonMaxBytes     = 4096
	verdictTaskTitleMaxBytes  = 256
	verdictMaxComments        = 50
	verdictMaxThreadDecisions = 100
	verdictFileMaxBytes       = 256 * 1024
	sealFileMaxBytes          = 16 * 1024
)

var decisionKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ReadFile retains the pre-completion behavior for custom process-exit checks:
// it validates the common schema without imposing a Flow-owned role mode.
func ReadFile(path string) (VerdictReport, bool, error) {
	validated, ok, err := ReadFileForMode(path, "")
	return validated.Report, ok, err
}

// ReadFileForMode reads the exact verdict bytes once, validates them, and
// computes the digest used by the completion seal.
func ReadFileForMode(path string, mode Mode) (ValidatedVerdict, bool, error) {
	data, ok, err := readLimitedFile(path, verdictFileMaxBytes, "verdict")
	if err != nil || !ok {
		return ValidatedVerdict{}, ok, err
	}
	validated, err := validateBytes(data, mode)
	if err != nil {
		return ValidatedVerdict{}, false, err
	}
	return validated, true, nil
}

func validateBytes(data []byte, mode Mode) (ValidatedVerdict, error) {
	report, err := Validate(data, mode)
	if err != nil {
		return ValidatedVerdict{}, err
	}
	sum := sha256.Sum256(data)
	return ValidatedVerdict{
		Report: report,
		Bytes:  data,
		Digest: hex.EncodeToString(sum[:]),
	}, nil
}

func Validate(data []byte, mode Mode) (VerdictReport, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return VerdictReport{}, fmt.Errorf("parse verdict file: %w", err)
	}
	var report VerdictReport
	if err := json.Unmarshal(data, &report); err != nil {
		return VerdictReport{}, fmt.Errorf("parse verdict file: %w", err)
	}
	switch report.Verdict {
	case "satisfied", "blocked":
	default:
		return VerdictReport{}, fmt.Errorf("invalid verdict %q (want satisfied|blocked)", report.Verdict)
	}
	if len(report.Reason) > verdictReasonMaxBytes {
		report.Reason = report.Reason[:verdictReasonMaxBytes]
	}
	if err := normalizeVerdictComments(&report); err != nil {
		return VerdictReport{}, err
	}
	if report.Verdict == "satisfied" {
		for i, comment := range report.Comments {
			if comment.BlocksApproval() {
				return VerdictReport{}, fmt.Errorf("verdict file: satisfied verdict includes blocking comment %d", i)
			}
		}
	}
	if err := normalizeVerdictThreads(&report); err != nil {
		return VerdictReport{}, err
	}
	if err := validateMode(fields, &report, mode); err != nil {
		return VerdictReport{}, err
	}
	return report, nil
}

func validateMode(fields map[string]json.RawMessage, report *VerdictReport, mode Mode) error {
	switch mode {
	case "":
		return nil
	case ModeReview, ModeReviewDiscovery:
		if _, present := fields["decision_request"]; present {
			return fmt.Errorf("verdict file: mode %s forbids decision_request", mode)
		}
		if _, present := fields["threads"]; present {
			return fmt.Errorf("verdict file: mode %s forbids threads", mode)
		}
		var rawComments []map[string]json.RawMessage
		if raw, present := fields["comments"]; present {
			if err := json.Unmarshal(raw, &rawComments); err != nil {
				return fmt.Errorf("parse verdict file comments: %w", err)
			}
		}
		for i, comment := range rawComments {
			if _, present := comment["task_action"]; present {
				return fmt.Errorf("verdict file: mode %s forbids comment %d task_action", mode, i)
			}
		}
		return validateReviewScopeMetadata(report, mode)
	case ModeReviewAggregation:
		if _, present := fields["threads"]; present {
			return errors.New("verdict file: mode review_aggregation forbids threads")
		}
		if err := validateReviewScopeMetadata(report, mode); err != nil {
			return err
		}
		return validateDecisionRequest(report)
	case ModeVerify:
		if _, present := fields["comments"]; present {
			return errors.New("verdict file: mode verify forbids comments")
		}
		if _, present := fields["decision_request"]; present {
			return errors.New("verdict file: mode verify forbids decision_request")
		}
	default:
		return fmt.Errorf("invalid check completion mode %q", mode)
	}
	return nil
}

func validateReviewScopeMetadata(report *VerdictReport, mode Mode) error {
	for index := range report.Comments {
		comment := &report.Comments[index]
		comment.RequirementSource = strings.ToLower(strings.TrimSpace(comment.RequirementSource))
		comment.FindingBasis = strings.ToLower(strings.TrimSpace(comment.FindingBasis))
		comment.RemediationScope = strings.ToLower(strings.TrimSpace(comment.RemediationScope))
		comment.ScopeRationale = strings.TrimSpace(comment.ScopeRationale)
		switch comment.RequirementSource {
		case "explicit", "inferred":
		default:
			return fmt.Errorf("verdict file: comment %d has invalid requirement_source %q (want explicit|inferred)", index, comment.RequirementSource)
		}
		switch comment.FindingBasis {
		case "explicit_requirement", "demonstrated_regression", "security_defect", "scope_inference":
		default:
			return fmt.Errorf("verdict file: comment %d has invalid finding_basis %q", index, comment.FindingBasis)
		}
		switch comment.RemediationScope {
		case "local", "cross_cutting", "legacy_migration", "unknown":
		default:
			return fmt.Errorf("verdict file: comment %d has invalid remediation_scope %q", index, comment.RemediationScope)
		}
		if comment.ScopeRationale == "" {
			return fmt.Errorf("verdict file: comment %d missing scope_rationale", index)
		}
		if len([]byte(comment.ScopeRationale)) > verdictReasonMaxBytes {
			return fmt.Errorf("verdict file: comment %d scope_rationale exceeds %d bytes", index, verdictReasonMaxBytes)
		}
		if comment.FindingBasis == "explicit_requirement" && comment.RequirementSource != "explicit" {
			return fmt.Errorf("verdict file: comment %d explicit_requirement must use explicit requirement_source", index)
		}
		if comment.FindingBasis == "scope_inference" && comment.RequirementSource != "inferred" {
			return fmt.Errorf("verdict file: comment %d scope_inference must use inferred requirement_source", index)
		}
	}
	_ = mode
	return nil
}

func requiresScopeDecision(comment ReviewCommentReport) bool {
	if !comment.BlocksApproval() || comment.RequirementSource != "inferred" || comment.FindingBasis != "scope_inference" {
		return false
	}
	switch comment.RemediationScope {
	case "cross_cutting", "legacy_migration", "unknown":
		return true
	default:
		return false
	}
}

func validateDecisionRequest(report *VerdictReport) error {
	required := map[int]struct{}{}
	for index, comment := range report.Comments {
		if requiresScopeDecision(comment) {
			required[index] = struct{}{}
		}
	}
	request := report.DecisionRequest
	if len(required) == 0 {
		if request != nil {
			return errors.New("verdict file: decision_request is forbidden without a blocking inferred scope finding")
		}
		return nil
	}
	if request == nil {
		return errors.New("verdict file: blocking inferred scope findings require decision_request")
	}
	request.Key = strings.TrimSpace(request.Key)
	request.Question = strings.TrimSpace(request.Question)
	request.Rationale = strings.TrimSpace(request.Rationale)
	if !decisionKeyPattern.MatchString(request.Key) {
		return errors.New("verdict file: decision_request key must match [a-z0-9][a-z0-9._-]{0,127}")
	}
	if request.Question == "" || len([]byte(request.Question)) > 1024 {
		return errors.New("verdict file: decision_request question must be between 1 and 1024 bytes")
	}
	if request.Rationale == "" || len([]byte(request.Rationale)) > verdictReasonMaxBytes {
		return fmt.Errorf("verdict file: decision_request rationale must be between 1 and %d bytes", verdictReasonMaxBytes)
	}
	if len(request.CommentIndexes) == 0 || len(request.CommentIndexes) > verdictMaxComments {
		return fmt.Errorf("verdict file: decision_request must reference between 1 and %d comments", verdictMaxComments)
	}
	seen := map[int]struct{}{}
	for _, index := range request.CommentIndexes {
		if index < 0 || index >= len(report.Comments) {
			return fmt.Errorf("verdict file: decision_request comment index %d is out of range", index)
		}
		if _, duplicate := seen[index]; duplicate {
			return fmt.Errorf("verdict file: decision_request comment index %d is duplicated", index)
		}
		seen[index] = struct{}{}
		if _, ok := required[index]; !ok {
			return fmt.Errorf("verdict file: decision_request comment index %d does not require a scope decision", index)
		}
	}
	for index := range required {
		if _, ok := seen[index]; !ok {
			return fmt.Errorf("verdict file: decision_request omits required comment index %d", index)
		}
	}
	for index, comment := range report.Comments {
		if comment.TaskAction != nil {
			return fmt.Errorf("verdict file: decision_request report forbids comment %d task_action", index)
		}
	}
	return nil
}

func normalizeVerdictComments(v *VerdictReport) error {
	if len(v.Comments) > verdictMaxComments {
		return fmt.Errorf("verdict file: %d comments exceeds cap of %d", len(v.Comments), verdictMaxComments)
	}
	for i := range v.Comments {
		c := &v.Comments[i]
		c.SHA = strings.TrimSpace(c.SHA)
		c.File = strings.TrimSpace(c.File)
		c.Body = strings.TrimSpace(c.Body)
		c.Severity = strings.ToLower(strings.TrimSpace(c.Severity))
		c.Requirement = strings.TrimSpace(c.Requirement)
		c.DuplicateOf = strings.TrimSpace(c.DuplicateOf)
		c.FollowUp = strings.TrimSpace(c.FollowUp)
		if c.SHA == "" {
			return fmt.Errorf("verdict file: comment %d missing sha", i)
		}
		if c.File == "" {
			return fmt.Errorf("verdict file: comment %d missing file", i)
		}
		if c.Line <= 0 {
			return fmt.Errorf("verdict file: comment %d line must be positive", i)
		}
		if c.Body == "" {
			return fmt.Errorf("verdict file: comment %d missing body", i)
		}
		switch c.Severity {
		case "critical", "high", "medium", "low":
		default:
			return fmt.Errorf("verdict file: comment %d has invalid severity %q (want critical|high|medium|low)", i, c.Severity)
		}
		if c.IntroducedByChange == nil {
			return fmt.Errorf("verdict file: comment %d missing introduced_by_change", i)
		}
		if c.Requirement == "" {
			return fmt.Errorf("verdict file: comment %d missing requirement", i)
		}
		if c.DuplicateOf != "" && !strings.HasPrefix(c.DuplicateOf, "th-") {
			return fmt.Errorf("verdict file: comment %d has invalid duplicate_of %q", i, c.DuplicateOf)
		}
		if len(c.Body) > verdictReasonMaxBytes {
			c.Body = c.Body[:verdictReasonMaxBytes]
		}
		if len(c.Requirement) > verdictReasonMaxBytes {
			c.Requirement = c.Requirement[:verdictReasonMaxBytes]
		}
		if len(c.FollowUp) > verdictReasonMaxBytes {
			c.FollowUp = c.FollowUp[:verdictReasonMaxBytes]
		}
		if err := normalizeReviewTaskAction(i, c); err != nil {
			return err
		}
	}
	return nil
}

func normalizeReviewTaskAction(index int, comment *ReviewCommentReport) error {
	action := comment.TaskAction
	if action == nil {
		return nil
	}
	action.Action = strings.TrimSpace(action.Action)
	action.Title = strings.TrimSpace(action.Title)
	action.Body = strings.TrimSpace(action.Body)
	action.TaskID = strings.TrimSpace(action.TaskID)
	if comment.BlocksApproval() {
		return fmt.Errorf("verdict file: comment %d blocking finding cannot declare task_action", index)
	}
	if comment.DuplicateOf != "" {
		return fmt.Errorf("verdict file: comment %d review-thread duplicate cannot declare task_action", index)
	}
	switch action.Action {
	case "create_task":
		if action.Title == "" {
			return fmt.Errorf("verdict file: comment %d create_task requires title", index)
		}
		if action.Body == "" {
			return fmt.Errorf("verdict file: comment %d create_task requires body", index)
		}
		if action.TaskID != "" {
			return fmt.Errorf("verdict file: comment %d create_task must not specify task_id", index)
		}
		if len(action.Title) > verdictTaskTitleMaxBytes {
			return fmt.Errorf("verdict file: comment %d task title exceeds %d bytes", index, verdictTaskTitleMaxBytes)
		}
		if len(action.Body) > verdictReasonMaxBytes {
			return fmt.Errorf("verdict file: comment %d task body exceeds %d bytes", index, verdictReasonMaxBytes)
		}
	case "use_existing_task":
		if action.TaskID == "" {
			return fmt.Errorf("verdict file: comment %d use_existing_task requires task_id", index)
		}
		if action.Title != "" || action.Body != "" {
			return fmt.Errorf("verdict file: comment %d use_existing_task must not specify title or body", index)
		}
	default:
		return fmt.Errorf("verdict file: comment %d has invalid task_action %q (want create_task|use_existing_task)", index, action.Action)
	}
	return nil
}

func normalizeVerdictThreads(v *VerdictReport) error {
	if len(v.Threads) > verdictMaxThreadDecisions {
		return fmt.Errorf("verdict file: %d thread decisions exceeds cap of %d", len(v.Threads), verdictMaxThreadDecisions)
	}
	for i := range v.Threads {
		d := &v.Threads[i]
		d.ID = strings.TrimSpace(d.ID)
		d.Decision = strings.TrimSpace(d.Decision)
		d.Body = strings.TrimSpace(d.Body)
		if d.ID == "" {
			return fmt.Errorf("verdict file: thread decision %d missing id", i)
		}
		switch d.Decision {
		case "certify", "reopen":
		default:
			return fmt.Errorf("verdict file: thread decision %d has invalid decision %q (want certify|reopen)", i, d.Decision)
		}
		if d.Decision == "reopen" && d.Body == "" {
			return fmt.Errorf("verdict file: thread decision %d reopen requires a body", i)
		}
		if len(d.Body) > verdictReasonMaxBytes {
			d.Body = d.Body[:verdictReasonMaxBytes]
		}
	}
	return nil
}

// SealVerdict validates the verdict for its role and atomically writes the
// immutable completion seal. An exact replay succeeds; changed content does not.
func SealVerdict(verdictPath, sealPath string, context Context) (ValidatedVerdict, error) {
	if err := validateContext(context); err != nil {
		return ValidatedVerdict{}, err
	}
	data, ok, err := readLimitedFile(verdictPath, verdictFileMaxBytes, "verdict")
	if err != nil {
		return ValidatedVerdict{}, err
	}
	if !ok {
		return ValidatedVerdict{}, fmt.Errorf("verdict file %q is missing", verdictPath)
	}
	sum := sha256.Sum256(data)
	expected := sealFor(context, hex.EncodeToString(sum[:]))
	existing, ok, err := ReadSeal(sealPath)
	if err != nil {
		return ValidatedVerdict{}, err
	}
	if ok {
		if existing == expected {
			return validateBytes(data, context.Mode)
		}
		return ValidatedVerdict{}, errors.New("completion seal is final; verdict or check context changed after completion")
	}
	validated, err := validateBytes(data, context.Mode)
	if err != nil {
		return ValidatedVerdict{}, err
	}
	if err := writeSealAtomic(sealPath, expected); err != nil {
		if errors.Is(err, fs.ErrExist) {
			existing, present, readErr := ReadSeal(sealPath)
			if readErr != nil {
				return ValidatedVerdict{}, readErr
			}
			if present && existing == expected {
				return validated, nil
			}
			return ValidatedVerdict{}, errors.New("completion seal is final; verdict or check context changed after completion")
		}
		return ValidatedVerdict{}, err
	}
	return validated, nil
}

// VerifySeal validates a worker-observed seal against authoritative job
// context and the exact current verdict bytes.
func VerifySeal(sealPath, verdictPath string, context Context) (ValidatedVerdict, bool, error) {
	if err := validateContext(context); err != nil {
		return ValidatedVerdict{}, false, err
	}
	seal, ok, err := ReadSeal(sealPath)
	if err != nil || !ok {
		return ValidatedVerdict{}, ok, err
	}
	validated, present, err := ReadFileForMode(verdictPath, context.Mode)
	if err != nil {
		return ValidatedVerdict{}, true, err
	}
	if !present {
		return ValidatedVerdict{}, true, fmt.Errorf("sealed verdict file %q is missing", verdictPath)
	}
	if seal != sealFor(context, validated.Digest) {
		return ValidatedVerdict{}, true, errors.New("completion seal does not match the job context and exact verdict contents")
	}
	return validated, true, nil
}

func ReadSeal(path string) (Seal, bool, error) {
	data, ok, err := readLimitedFile(path, sealFileMaxBytes, "completion seal")
	if err != nil || !ok {
		return Seal{}, ok, err
	}
	var seal Seal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&seal); err != nil {
		return Seal{}, false, fmt.Errorf("parse completion seal: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Seal{}, false, fmt.Errorf("parse completion seal: trailing data: %w", err)
	}
	if seal.Version != SealVersion {
		return Seal{}, false, fmt.Errorf("completion seal version %d is unsupported", seal.Version)
	}
	if _, err := ParseMode(string(seal.Mode)); err != nil {
		return Seal{}, false, err
	}
	if strings.TrimSpace(seal.JobID) == "" || strings.TrimSpace(seal.CheckName) == "" {
		return Seal{}, false, errors.New("completion seal job_id and check_name are required")
	}
	if len(seal.VerdictSHA256) != sha256.Size*2 {
		return Seal{}, false, errors.New("completion seal verdict_sha256 must be a lowercase SHA-256 digest")
	}
	if decoded, err := hex.DecodeString(seal.VerdictSHA256); err != nil || hex.EncodeToString(decoded) != seal.VerdictSHA256 {
		return Seal{}, false, errors.New("completion seal verdict_sha256 must be a lowercase SHA-256 digest")
	}
	return seal, true, nil
}

func sealFor(context Context, digest string) Seal {
	return Seal{
		Version:       SealVersion,
		JobID:         strings.TrimSpace(context.JobID),
		CheckName:     strings.TrimSpace(context.CheckName),
		Mode:          context.Mode,
		VerdictSHA256: digest,
	}
}

func validateContext(context Context) error {
	if strings.TrimSpace(context.JobID) == "" {
		return errors.New("check completion job id is required")
	}
	if strings.TrimSpace(context.CheckName) == "" {
		return errors.New("check completion check name is required")
	}
	_, err := ParseMode(string(context.Mode))
	return err
}

func writeSealAtomic(path string, seal Seal) error {
	data, err := json.MarshalIndent(seal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode completion seal: %w", err)
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create completion seal directory: %w", err)
	}
	file, err := os.CreateTemp(directory, ".flow-completion-*")
	if err != nil {
		return fmt.Errorf("create completion seal: %w", err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod completion seal: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write completion seal: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync completion seal: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close completion seal: %w", err)
	}
	// Linking a fully-written temporary file publishes it atomically without
	// replacing a seal another completion attempt may have created.
	if err := os.Link(tempPath, path); err != nil {
		return fmt.Errorf("publish completion seal: %w", err)
	}
	return nil
}

func readLimitedFile(path string, limit int64, label string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, false, fmt.Errorf("parse %s file: exceeds %d bytes", label, limit)
	}
	return data, true, nil
}
