package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func ResolveThreadIDsFromMessage(ctx context.Context, message string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "interpret-trailers", "--parse")
	cmd.Stdin = strings.NewReader(message)
	var output bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}

	return parseResolveTrailerOutput(output.String()), nil
}

func parseResolveTrailerOutput(output string) []string {
	seen := map[string]bool{}
	var ids []string
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "Resolves") {
			continue
		}
		for _, item := range strings.Split(value, ",") {
			id := strings.TrimSpace(item)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}

	return ids
}
