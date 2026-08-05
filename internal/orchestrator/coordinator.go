package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

const assignmentsPath = "/v2/provisioner/assignments"
const capacitySlotsPath = "/v2/provisioner/capacity-slots"
const capacityDemandPath = "/v2/provisioner/capacity-demand"

// Coordinator is the typed durable-assignment API used by the Reconciler.
type Coordinator interface {
	ListAssignments(context.Context, []string) ([]contract.ProvisionerAssignment, error)
	Reserve(context.Context, contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error)
	RecordAttempt(context.Context, string, contract.RecordProvisionerAssignmentAttemptRequest) (contract.ProvisionerAssignment, error)
	Abandon(context.Context, string, contract.AbandonProvisionerAssignmentRequest) (contract.ProvisionerAssignment, error)
	Revoke(context.Context, string) (contract.ProvisionerAssignment, error)
	Cleaned(context.Context, string) (contract.ProvisionerAssignment, error)
}

// CapacityCoordinator is the slot-first API used by production reconcilers.
// It is separate so focused assignment tests can keep small fakes.
type CapacityCoordinator interface {
	ListCapacitySlots(context.Context, []string) ([]worker.CapacitySlot, error)
	CreateCapacitySlot(context.Context, contract.CreateProvisionerCapacitySlotRequest) (contract.CreateProvisionerCapacitySlotResponse, error)
	CapacityDemand(context.Context, contract.ProvisionerCapacityDemandRequest) (contract.ProvisionerCapacityDemandResponse, error)
	BindCapacitySlot(context.Context, string, contract.BindProvisionerCapacitySlotRequest) (contract.BindProvisionerCapacitySlotResponse, error)
	RecordCapacitySlotAttempt(context.Context, string, contract.RecordProvisionerCapacitySlotAttemptRequest) (worker.CapacitySlot, error)
	CloseCapacitySlot(context.Context, string, contract.CloseProvisionerCapacitySlotRequest) (worker.CapacitySlot, error)
	RevokeCapacitySlot(context.Context, string) (worker.CapacitySlot, error)
	CleanCapacitySlot(context.Context, string) (worker.CapacitySlot, error)
}

// APIError describes a non-2xx response from the coordinator.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	detail := strings.TrimSpace(e.Message)
	if detail == "" {
		detail = http.StatusText(e.StatusCode)
	}
	if e.Code != "" {
		detail = e.Code + ": " + detail
	}
	return fmt.Sprintf("coordinator %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, detail)
}

// CoordinatorClient implements Coordinator over the provisioner HTTP API.
type CoordinatorClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewCoordinatorClient constructs an assignment client. A nil HTTP client uses
// a client with a ten-second timeout.
func NewCoordinatorClient(baseURL, token string, client *http.Client) *CoordinatorClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &CoordinatorClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		client:  client,
	}
}

// BaseURL is the coordinator URL suitable for generated worker configuration.
func (c *CoordinatorClient) BaseURL() string { return c.baseURL }

func (c *CoordinatorClient) ListAssignments(ctx context.Context, _ []string) ([]contract.ProvisionerAssignment, error) {
	var assignments []contract.ProvisionerAssignment
	seenAssignments := make(map[string]struct{})
	for _, filter := range []string{"open_only=true", "needs_cleanup=true"} {
		var response contract.ProvisionerAssignmentsResponse
		path := assignmentsPath + "?" + filter
		if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, err
		}
		for _, assignment := range response.Assignments {
			key := assignment.Project.ID + "\x00" + assignment.Assignment.ID
			if _, ok := seenAssignments[key]; ok {
				continue
			}
			seenAssignments[key] = struct{}{}
			assignments = append(assignments, assignment)
		}
	}
	return assignments, nil
}

func (c *CoordinatorClient) ListCapacitySlots(ctx context.Context, _ []string) ([]worker.CapacitySlot, error) {
	var slots []worker.CapacitySlot
	seen := make(map[string]bool)
	for _, filter := range []string{"open_only=true", "needs_cleanup=true"} {
		var response contract.ProvisionerCapacitySlotsResponse
		if err := c.do(ctx, http.MethodGet, capacitySlotsPath+"?"+filter, nil, &response); err != nil {
			return nil, err
		}
		for _, slot := range response.Slots {
			if !seen[slot.ID] {
				slots = append(slots, slot)
				seen[slot.ID] = true
			}
		}
	}
	return slots, nil
}

func (c *CoordinatorClient) CreateCapacitySlot(ctx context.Context, request contract.CreateProvisionerCapacitySlotRequest) (contract.CreateProvisionerCapacitySlotResponse, error) {
	var response contract.CreateProvisionerCapacitySlotResponse
	err := c.do(ctx, http.MethodPost, capacitySlotsPath, request, &response)
	return response, err
}

func (c *CoordinatorClient) CapacityDemand(ctx context.Context, request contract.ProvisionerCapacityDemandRequest) (contract.ProvisionerCapacityDemandResponse, error) {
	var response contract.ProvisionerCapacityDemandResponse
	err := c.do(ctx, http.MethodPost, capacityDemandPath, request, &response)
	return response, err
}

func (c *CoordinatorClient) BindCapacitySlot(ctx context.Context, id string, request contract.BindProvisionerCapacitySlotRequest) (contract.BindProvisionerCapacitySlotResponse, error) {
	var response contract.BindProvisionerCapacitySlotResponse
	err := c.do(ctx, http.MethodPost, capacitySlotsPath+"/"+url.PathEscape(strings.TrimSpace(id))+"/bind", request, &response)
	return response, err
}

func (c *CoordinatorClient) RecordCapacitySlotAttempt(ctx context.Context, id string, request contract.RecordProvisionerCapacitySlotAttemptRequest) (worker.CapacitySlot, error) {
	var response contract.ProvisionerCapacitySlotResponse
	err := c.do(ctx, http.MethodPost, capacitySlotsPath+"/"+url.PathEscape(strings.TrimSpace(id))+"/attempt", request, &response)
	return response.Slot, err
}

func (c *CoordinatorClient) CloseCapacitySlot(ctx context.Context, id string, request contract.CloseProvisionerCapacitySlotRequest) (worker.CapacitySlot, error) {
	var response contract.ProvisionerCapacitySlotResponse
	err := c.do(ctx, http.MethodPost, capacitySlotsPath+"/"+url.PathEscape(strings.TrimSpace(id))+"/close", request, &response)
	return response.Slot, err
}

func (c *CoordinatorClient) RevokeCapacitySlot(ctx context.Context, id string) (worker.CapacitySlot, error) {
	var response contract.ProvisionerCapacitySlotResponse
	err := c.do(ctx, http.MethodPost, capacitySlotsPath+"/"+url.PathEscape(strings.TrimSpace(id))+"/revoked", struct{}{}, &response)
	return response.Slot, err
}

func (c *CoordinatorClient) CleanCapacitySlot(ctx context.Context, id string) (worker.CapacitySlot, error) {
	var response contract.ProvisionerCapacitySlotResponse
	err := c.do(ctx, http.MethodPost, capacitySlotsPath+"/"+url.PathEscape(strings.TrimSpace(id))+"/cleaned", struct{}{}, &response)
	return response.Slot, err
}

func (c *CoordinatorClient) Reserve(ctx context.Context, request contract.ReserveProvisionerAssignmentRequest) (contract.ReserveProvisionerAssignmentResponse, error) {
	var response contract.ReserveProvisionerAssignmentResponse
	err := c.do(ctx, http.MethodPost, assignmentsPath+"/reserve", request, &response)
	return response, err
}

func (c *CoordinatorClient) RecordAttempt(ctx context.Context, assignmentID string, request contract.RecordProvisionerAssignmentAttemptRequest) (contract.ProvisionerAssignment, error) {
	var response contract.ProvisionerAssignmentResponse
	path := assignmentsPath + "/" + url.PathEscape(strings.TrimSpace(assignmentID)) + "/attempt"
	err := c.do(ctx, http.MethodPost, path, request, &response)
	return response.Assignment, err
}

func (c *CoordinatorClient) Abandon(ctx context.Context, assignmentID string, request contract.AbandonProvisionerAssignmentRequest) (contract.ProvisionerAssignment, error) {
	var response contract.ProvisionerAssignmentResponse
	path := assignmentsPath + "/" + url.PathEscape(strings.TrimSpace(assignmentID)) + "/abandon"
	err := c.do(ctx, http.MethodPost, path, request, &response)
	return response.Assignment, err
}

func (c *CoordinatorClient) Revoke(ctx context.Context, assignmentID string) (contract.ProvisionerAssignment, error) {
	var response contract.ProvisionerAssignmentResponse
	path := assignmentsPath + "/" + url.PathEscape(strings.TrimSpace(assignmentID)) + "/revoked"
	err := c.do(ctx, http.MethodPost, path, struct{}{}, &response)
	return response.Assignment, err
}

func (c *CoordinatorClient) Cleaned(ctx context.Context, assignmentID string) (contract.ProvisionerAssignment, error) {
	var response contract.ProvisionerAssignmentResponse
	path := assignmentsPath + "/" + url.PathEscape(strings.TrimSpace(assignmentID)) + "/cleaned"
	err := c.do(ctx, http.MethodPost, path, struct{}{}, &response)
	return response.Assignment, err
}

func (c *CoordinatorClient) do(ctx context.Context, method, path string, body, target any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode coordinator request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build coordinator request: %w", err)
	}
	req.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	if c.token != "" {
		req.Header.Set("Authorization", contract.AuthScheme+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("coordinator %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 4<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var response contract.ErrorResponse
		data, _ := io.ReadAll(limited)
		_ = json.Unmarshal(data, &response)
		message := response.Error.Message
		if message == "" {
			message = strings.TrimSpace(string(data))
		}
		return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Code: response.Error.Code, Message: message}
	}
	if target == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, limited)
		return nil
	}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode coordinator %s %s response: %w", method, path, err)
	}
	return nil
}
