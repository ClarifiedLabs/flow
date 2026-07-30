package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// CoordinatorPoller is a StatsSource that GETs the coordinator's
// /v2/queue/stats endpoint with the orchestrator-scoped bearer token.
type CoordinatorPoller struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewCoordinatorPoller builds a poller for the coordinator at baseURL. A nil
// client uses a default client with a 10s timeout.
func NewCoordinatorPoller(baseURL string, token string, client *http.Client) *CoordinatorPoller {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &CoordinatorPoller{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  client,
	}
}

func (p *CoordinatorPoller) QueueStats(ctx context.Context) (QueueStats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v2/queue/stats", nil)
	if err != nil {
		return QueueStats{}, fmt.Errorf("build queue stats request: %w", err)
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return QueueStats{}, fmt.Errorf("get queue stats: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return QueueStats{}, fmt.Errorf("get queue stats: unexpected status %s", resp.Status)
	}
	var body struct {
		Queued           int `json:"queued"`
		ClaimedOrRunning int `json:"claimed_or_running"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return QueueStats{}, fmt.Errorf("decode queue stats: %w", err)
	}

	return QueueStats{Queued: body.Queued, ClaimedOrRunning: body.ClaimedOrRunning}, nil
}
