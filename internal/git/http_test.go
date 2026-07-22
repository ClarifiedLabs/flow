package git

import "testing"

func TestExchangeHTTPURLUsesCallersServerURL(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		serverURL string
		want      string
	}{
		{
			name:      "host worker",
			serverURL: "http://127.0.0.1:8421",
			want:      "http://127.0.0.1:8421/git/projects/p-flow/exchange.git",
		},
		{
			name:      "compose worker",
			serverURL: "http://flow-server:8421/",
			want:      "http://flow-server:8421/git/projects/p-flow/exchange.git",
		},
		{
			name:      "reverse proxy path",
			serverURL: "https://flow.example.test/service",
			want:      "https://flow.example.test/service/git/projects/p-flow/exchange.git",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExchangeHTTPURL(tc.serverURL, "p-flow")
			if err != nil {
				t.Fatalf("ExchangeHTTPURL: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ExchangeHTTPURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExchangeHTTPURLRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		serverURL string
		projectID string
	}{
		{name: "missing server", projectID: "p-flow"},
		{name: "relative server", serverURL: "flow-server:8421", projectID: "p-flow"},
		{name: "unsupported scheme", serverURL: "file:///tmp/flow", projectID: "p-flow"},
		{name: "missing project", serverURL: "http://flow-server:8421"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ExchangeHTTPURL(tc.serverURL, tc.projectID); err == nil {
				t.Fatal("ExchangeHTTPURL accepted invalid input")
			}
		})
	}
}
