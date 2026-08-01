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
//
// HeadSHA is the commit the reviewer actually inspected. The submission is
// rejected with a conflict if the change advanced past it, so inline threads
// and the verdict stay bound to the code the reviewer saw rather than to a
// newer head they never looked at.
type reviewVerdictRequest struct {
	Verdict  string                `json:"verdict"`
	HeadSHA  string                `json:"head_sha"`
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
	for _, comment := range request.Comments {
		if strings.TrimSpace(comment.Body) == "" {
			writeError(w, http.StatusBadRequest, "invalid_comment", "inline comment body is required")
			return
		}
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

	comments := make([]coordinator.SubmitReviewComment, 0, len(request.Comments))
	for _, comment := range request.Comments {
		comments = append(comments, coordinator.SubmitReviewComment{
			FilePath: comment.FilePath,
			Line:     comment.Line,
			Anchor:   comment.AnchorCommitSHA,
			Context:  comment.Context,
			Body:     comment.Body,
		})
	}
	result, err := s.threads.SubmitReview(ctx, coordinator.SubmitReviewInput{
		ChangeID:  changeID,
		HeadSHA:   request.HeadSHA,
		Verdict:   verdict,
		Body:      request.Body,
		CheckName: humanReviewCheckName,
		Comments:  comments,
		Actor:     principal.Actor(),
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "change_not_found", "change not found")
		return
	case errors.Is(err, coordinator.ErrReviewHeadMoved):
		writeError(w, http.StatusConflict, "head_moved", "change head moved since this review was rendered; reload the change and re-review")
		return
	case err != nil:
		writeWorkflowError(w, err, "review_failed")
		return
	}

	response := reviewVerdictResponse{Verdict: verdict, Threads: result.Threads}
	if result.Check != nil {
		response.Check = result.Check
	}
	// A bare comment records notes without moving the review forward, so it
	// must not nudge the executor either: on a scheduled run (or a running run
	// without a current node) Advance would start the workflow, enter the
	// human gate, or dispatch work. Only a verdict responds to the gate.
	if verdict != "comment" {
		s.advanceWorkflowForTask(w, r, taskID)
	}
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
