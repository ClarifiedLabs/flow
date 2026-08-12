package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
	"github.com/ClarifiedLabs/flow/internal/config"
)

// newTestClientPair stands up the MCP server over an in-memory transport and
// returns a connected client session.
func newTestClientPair(t *testing.T, client *flowclient.Client) *sdk.ClientSession {
	t.Helper()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	server := NewServer(client)
	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	mcpClient := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestToolCatalogIsReadOnlyAllowlist(t *testing.T) {
	// A nil-backed client is fine here: catalog listing never calls tools.
	client, err := flowclient.New(config.ClientConfig{ServerURL: "http://127.0.0.1:1", Token: "t"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	session := newTestClientPair(t, client)

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"flow.events", "flow.ready", "flow.search", "flow.task_list", "flow.task_show"}
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want exactly %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tools = %v, want %v", names, want)
		}
	}
}

func TestTaskListToolCallsServer(t *testing.T) {
	// A stub flow-server that answers the project task list with one task.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tasks": []map[string]any{{"id": "t-demo-0001", "title": "Demo task", "state": "scheduled"}},
		})
	}))
	t.Cleanup(server.Close)

	client, err := flowclient.New(config.ClientConfig{ServerURL: server.URL, Token: "owner-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	session := newTestClientPair(t, client)

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "flow.task_list",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call task_list: %v", err)
	}
	if result.IsError {
		t.Fatalf("task_list returned error: %+v", result.Content)
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var payload toolResult
	if err := json.Unmarshal(structured, &payload); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	data, _ := json.Marshal(payload.Data)
	if !json.Valid(data) || !bytes.Contains(data, []byte("t-demo-0001")) {
		t.Fatalf("task_list data = %s", data)
	}
}

func TestTaskShowRequiresID(t *testing.T) {
	client, err := flowclient.New(config.ClientConfig{ServerURL: "http://127.0.0.1:1", Token: "t"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	session := newTestClientPair(t, client)
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "flow.task_show",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call task_show: %v", err)
	}
	if !result.IsError {
		t.Fatalf("task_show without id should be an error result")
	}
}
