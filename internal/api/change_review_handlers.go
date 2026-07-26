package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// humanReviewCheckName is the required check a human reviewer's verdict
// reports against. The web UI's Approve button has always targeted it; the
// review bar batches inline notes into the same report.
const humanReviewCheckName = "human-review"

// reviewVerdictRequest is one submitted review: any inline notes the reviewer
// drafted while reading the diff, plus the overall verdict they are posting
// with them. Posting the notes and the verdict separately is what made the old
// single-box review lossy.
type reviewVerdictRequest struct {
	Verdict  string                `json:"verdict"`
	Body     string                `json:"body,omitempty"`
	Comments []reviewInlineComment `json:"comments,omitempty"`
}

type reviewInlineComment struct {
	FilePath        string `json:"file_path"`
	Line            int    `json:"line"`
	AnchorCommitSHA string `json:"anchor_commit_sha,omitempty"`
	Context         string `json:"context,omitempty"`
	Body            string `json:"body"`
}

type reviewVerdictResponse struct {
	Threads []coordinator.ReviewThread `json:"threads,omitempty"`
	Check   *coordinator.Check         `json:"check,omitempty"`
	Verdict string                     `json:"verdict"`
}

func (s *projectServer) handleSubmitReview(w http.ResponseWriter, r *http.Request, principal coordinator.Principal, changeID string) {
	if s.threads == nil || s.checks == nil {
		writeError(w, http.StatusInternalServerError, "review_unavailable", "review services are not configured")
		return
	}
	var request reviewVerdictRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	verdict := strings.TrimSpace(request.Verdict)
	switch verdict {
	case "approve", "request_changes", "comment":
	default:
		writeError(w, http.StatusBadRequest, "invalid_verdict", `verdict must be approve, request_changes, or comment`)
		return
	}

	ctx := r.Context()
	taskID, err := s.threads.ChangeTaskID(ctx, changeID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "load_change_failed", err.Error())
		return
	}
	change, err := s.sessions.GetChange(ctx, changeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "load_change_failed", err.Error())
		return
	}

	response := reviewVerdictResponse{Verdict: verdict}
	for _, comment := range request.Comments {
		if strings.TrimSpace(comment.Body) == "" {
			writeError(w, http.StatusBadRequest, "invalid_comment", "inline comment body is required")
			return
		}
		anchor := strings.TrimSpace(comment.AnchorCommitSHA)
		if anchor == "" {
			anchor = change.HeadSHA
		}
		thread, err := s.threads.CreateThread(ctx, coordinator.CreateThreadInput{
			ChangeID:        changeID,
			AnchorCommitSHA: anchor,
			FilePath:        comment.FilePath,
			Line:            comment.Line,
			Context:         comment.Context,
			Body:            comment.Body,
			Actor:           principal.Actor(),
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "create_thread_failed", err.Error())
			return
		}
		response.Threads = append(response.Threads, thread)
	}

	// A bare comment records the notes without moving the review forward.
	if verdict == "comment" {
		writeJSON(w, http.StatusOK, response)
		return
	}

	checkVerdict := coordinator.CheckSatisfied
	if verdict == "request_changes" {
		checkVerdict = coordinator.CheckBlocked
	}
	required := true
	check, err := s.checks.ReportCheck(ctx, coordinator.ReportCheckInput{
		TaskID:   taskID,
		Name:     humanReviewCheckName,
		Kind:     coordinator.CheckKindHuman,
		Required: &required,
		Verdict:  checkVerdict,
		Details:  strings.TrimSpace(request.Body),
		Reporter: string(principal.Actor()),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "report_check_failed", err.Error())
		return
	}
	response.Check = &check

	s.advanceWorkflowForTask(w, r, taskID)
	writeJSON(w, http.StatusOK, response)
}

// advanceWorkflowForTask nudges the executor after a human input so the run
// does not sit idle until the next tick. Failures are not fatal to the caller's
// write: the tick will retry.
func (s *projectServer) advanceWorkflowForTask(_ http.ResponseWriter, r *http.Request, taskID string) {
	if s.workflowExecutor == nil || s.workflowRuns == nil {
		return
	}
	run, active, err := s.workflowRuns.ActiveForTask(r.Context(), taskID)
	if err != nil || !active {
		return
	}
	_ = s.workflowExecutor.Advance(r.Context(), run.ID)
}

func (s *projectServer) handleMergeChange(w http.ResponseWriter, r *http.Request, changeID string) {
	if s.merges == nil {
		writeError(w, http.StatusInternalServerError, "merges_unavailable", "merge service is not configured")
		return
	}
	result, err := s.merges.MergeChange(r.Context(), changeID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "merge_failed", err.Error())
		return
	}
	s.advanceWorkflowForTask(w, r, result.Task.ID)
	writeJSON(w, http.StatusOK, mergeResponse{Merge: result})
}
