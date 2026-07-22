package git

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const ExchangeHTTPPrefix = "/git/projects/"

// ExchangeHTTPPath returns the stable HTTP route for a project's exchange.
func ExchangeHTTPPath(projectID string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "", errors.New("project id is required")
	}

	return ExchangeHTTPPrefix + url.PathEscape(projectID) + "/exchange.git", nil
}

// ExchangeHTTPURL resolves a project's exchange route against the Flow server
// URL reachable by the caller. The caller owns the base URL because different
// clients and workers can reach the same server through different networks.
func ExchangeHTTPURL(serverURL string, projectID string) (string, error) {
	serverURL = strings.TrimSpace(serverURL)
	if serverURL == "" {
		return "", errors.New("server url is required")
	}
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", fmt.Errorf("parse server url: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("server url must be an absolute HTTP(S) URL: %q", serverURL)
	}

	path, err := ExchangeHTTPPath(projectID)
	if err != nil {
		return "", err
	}
	joined, err := url.JoinPath(strings.TrimRight(serverURL, "/"), strings.TrimPrefix(path, "/"))
	if err != nil {
		return "", fmt.Errorf("build exchange url: %w", err)
	}

	return joined, nil
}
