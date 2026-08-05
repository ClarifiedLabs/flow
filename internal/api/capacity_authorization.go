package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func (s *Server) authorizeCapacityWorkerRequest(ctx context.Context, request *http.Request, principal coordinator.Principal) error {
	if principal.Scope != coordinator.TokenScopeWorker {
		return nil
	}
	path := request.URL.Path
	if path == "/v2/workers/register" || path == "/v2/workers/heartbeat" || path == "/v2/workers/claim" || path == "/v2/workers/reap-jobs" || strings.HasSuffix(path, "/control") {
		return nil
	}
	slot, err := s.registry.CapacitySlots().FindByWorker(ctx, principal.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Direct/unmanaged worker credentials retain their existing policy.
	}
	if err != nil {
		return err
	}
	if slot.State != worker.CapacitySlotBound || slot.ProjectID == nil || slot.AssignmentID == nil {
		return errors.New("idle capacity worker credential is not authorized for job or project APIs")
	}
	if path == "/v2/jobs" {
		return errors.New("capacity worker credential cannot list jobs")
	}
	projectID := strings.TrimSpace(*slot.ProjectID)
	if strings.HasPrefix(path, "/v2/projects/") {
		remainder := strings.TrimPrefix(path, "/v2/projects/")
		requested, _, _ := strings.Cut(remainder, "/")
		if requested != projectID {
			return errors.New("capacity worker credential is not authorized for this project")
		}
	}
	for _, key := range []string{"project", "project_id"} {
		if requested := strings.TrimSpace(request.URL.Query().Get(key)); requested != "" && requested != projectID {
			return errors.New("capacity worker credential is not authorized for this project")
		}
	}
	if strings.HasPrefix(path, "/v2/jobs/") {
		jobID, _, _ := strings.Cut(strings.TrimPrefix(path, "/v2/jobs/"), "/")
		record, findErr := s.registry.GetProvisionerAssignment(ctx, *slot.AssignmentID)
		if findErr != nil {
			return findErr
		}
		if strings.TrimSpace(jobID) != record.Assignment.JobID {
			return fmt.Errorf("capacity worker credential is not authorized for job %s", jobID)
		}
	}
	return nil
}
