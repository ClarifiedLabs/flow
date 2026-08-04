package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestHistoryListStrictFiltersAndJSON(t *testing.T) {
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/projects/project-1/history/captures" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"snapshot_until":"2026-08-03T12:00:00Z","captures":[{"id":"capture-1","project_id":"project-1","job_id":"job-1","task_id":"task-1","role":"implementation","state":"complete","resumable":true,"reserved_at":"2026-08-03T10:00:00Z","updated_at":"2026-08-03T11:00:00Z"}],"availability":{"total":1,"complete":1,"resumable":1,"blocked":0,"lost":0,"waived":0}}`)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := runHistory([]string{
		"list", "--server", server.URL, "--token", "owner", "--project", "project-1",
		"--task-id", "task-1", "--task-id", "task-2", "--job-id", "job-1",
		"--state", "complete", "--since", "2026-08-03T09:00:00-04:00",
		"--until", "2026-08-04T00:00:00Z", "--resumable=false", "--limit", "17", "--format", "json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runHistory(list) code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := gotQuery["task_id"], []string{"task-1", "task-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("task_id = %#v, want %#v", got, want)
	}
	for key, want := range map[string]string{
		"job_id": "job-1", "state": "complete", "since": "2026-08-03T13:00:00Z",
		"until": "2026-08-04T00:00:00Z", "resumable": "false", "limit": "17",
	} {
		if got := gotQuery.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	var response struct {
		Captures []struct {
			ID string `json:"id"`
		} `json:"captures"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, stdout.String())
	}
	if len(response.Captures) != 1 || response.Captures[0].ID != "capture-1" {
		t.Fatalf("JSON output captures = %#v", response.Captures)
	}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "bad state", args: []string{"list", "--state", "finished"}},
		{name: "bad clock", args: []string{"list", "--since", "yesterday"}},
		{name: "reversed interval", args: []string{"list", "--since", "2026-08-04T00:00:00Z", "--until", "2026-08-03T00:00:00Z"}},
		{name: "zero limit", args: []string{"list", "--limit", "0"}},
		{name: "bad format", args: []string{"list", "--format", "yaml"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := runHistory(test.args, &out, &errOut); got != 2 {
				t.Fatalf("code = %d, want 2; stderr = %s", got, errOut.String())
			}
		})
	}
}

func TestHistoryExportDeterministicBundlesAndIncompletePolicy(t *testing.T) {
	manifestBody := []byte("{\"format\":\"flow-history-manifest\",\"schema_version\":1}\n")
	artifactBody := []byte("committed artifact bytes\n")
	manifestDigest := historyTestDigest(manifestBody)
	artifactDigest := historyTestDigest(artifactBody)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/v2/projects/project-1/history/captures"
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base && r.URL.Query().Get("cursor") == "":
			if r.URL.Query().Get("limit") != "200" {
				t.Errorf("list limit = %q, want 200", r.URL.Query().Get("limit"))
			}
			fmt.Fprint(w, `{"snapshot_until":"2026-08-03T12:00:00Z","captures":[`+
				`{"id":"capture-z","project_id":"project-1","job_id":"job-z","role":"implementation","state":"lost","resumable":false,"reserved_at":"2026-08-03T11:00:00Z","updated_at":"2026-08-03T11:30:00Z"},`+
				`{"id":"capture-a","project_id":"project-1","job_id":"job-a","role":"implementation","state":"complete","resumable":true,"reserved_at":"2026-08-03T10:00:00Z","updated_at":"2026-08-03T11:00:00Z","completed_at":"2026-08-03T11:00:00Z"}`+
				`],"next_cursor":"next-page","availability":{"total":2,"complete":1,"resumable":1,"blocked":0,"lost":1,"waived":0}}`)
		case r.Method == http.MethodGet && r.URL.Path == base && r.URL.Query().Get("cursor") == "next-page":
			fmt.Fprint(w, `{"snapshot_until":"2026-08-03T12:00:00Z","captures":[],"availability":{"total":2,"complete":1,"resumable":1,"blocked":0,"lost":1,"waived":0}}`)
		case r.Method == http.MethodGet && r.URL.Path == base+"/capture-a":
			fmt.Fprintf(w, `{"capture":{"id":"capture-a","project_id":"project-1","job_id":"job-a","role":"implementation","state":"complete","resumable":true,"reserved_at":"2026-08-03T10:00:00Z","updated_at":"2026-08-03T11:00:00Z","completed_at":"2026-08-03T11:00:00Z"},"artifacts":[`+
				`{"id":"artifact-z","capture_id":"capture-a","logical_key":"payload","kind":"harness_root","phase":"final","media_type":"application/octet-stream","format_version":1,"schema_version":1,"sha256":%q,"stored_size":%d,"logical_size":%d,"entry_count":1,"publication_state":"committed","created_at":"2026-08-03T11:00:00Z"},`+
				`{"id":"artifact-a","capture_id":"capture-a","logical_key":"manifest","kind":"manifest","phase":"final","media_type":"application/json","format_version":1,"schema_version":1,"sha256":%q,"stored_size":%d,"logical_size":%d,"entry_count":1,"publication_state":"committed","created_at":"2026-08-03T11:00:00Z"}`+
				`],"harness_members":[]}`, artifactDigest, len(artifactBody), len(artifactBody), manifestDigest, len(manifestBody), len(manifestBody))
		case r.Method == http.MethodGet && r.URL.Path == base+"/capture-a/events":
			fmt.Fprint(w, `{"events":[`+
				`{"id":"event-z","capture_id":"capture-a","event_kind":"completed","capture_version":2,"actor":"worker","details":{},"occurred_at":"2026-08-03T11:00:00Z"},`+
				`{"id":"event-a","capture_id":"capture-a","event_kind":"reserved","capture_version":1,"actor":"server","details":{},"occurred_at":"2026-08-03T10:00:00Z"}`+
				`]}`)
		case r.Method == http.MethodGet && r.URL.Path == base+"/capture-a/manifest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifestBody)
		case r.Method == http.MethodGet && r.URL.Path == base+"/capture-a/artifacts/artifact-a/content":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(manifestBody)
		case r.Method == http.MethodGet && r.URL.Path == base+"/capture-a/artifacts/artifact-z/content":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(artifactBody)
		default:
			http.Error(w, r.Method+" "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	runExport := func(dir string, allowIncomplete bool) (int, string, string) {
		args := []string{"export", "--server", server.URL, "--token", "owner", "--project", "project-1", "--all", "--output", dir, "--parallel", "2", "--retries", "1"}
		if allowIncomplete {
			args = append(args, "--allow-incomplete")
		}
		var stdout, stderr bytes.Buffer
		code := runHistory(args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	firstDir := filepath.Join(t.TempDir(), "first")
	code, _, stderr := runExport(firstDir, false)
	if code != 1 || !strings.Contains(stderr, "capture state is lost") || strings.Contains(stderr, "export history capture capture-a") {
		t.Fatalf("export without allow-incomplete = %d, stderr = %s", code, stderr)
	}
	info, err := os.Stat(firstDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("output mode = %04o, want 0700", got)
	}
	marker, err := os.ReadFile(filepath.Join(firstDir, historyExportMarkerName))
	if err != nil || string(marker) != historyExportMarker {
		t.Fatalf("marker = %q, err = %v", marker, err)
	}

	bundleEntries, headers := readHistoryTestTar(t, filepath.Join(firstDir, "20260803T100000.000000000Z-capture-a.tar"))
	wantNames := []string{"capture.json", "capture-events.ndjson", "artifacts.json", "canonical-manifest.json", "artifacts/artifact-a.bin", "artifacts/artifact-z.bin"}
	if got := historyTestHeaderNames(headers); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("tar names = %#v, want %#v", got, wantNames)
	}
	for _, header := range headers {
		if header.Mode != 0o600 || !header.ModTime.Equal(deterministicHistoryTarHeader("x", 0).ModTime) {
			t.Errorf("non-deterministic header for %s: mode=%04o time=%s", header.Name, header.Mode, header.ModTime)
		}
	}
	if got := bundleEntries["canonical-manifest.json"]; !bytes.Equal(got, manifestBody) {
		t.Errorf("canonical manifest = %q, want %q", got, manifestBody)
	}
	if got := bundleEntries["artifacts/artifact-a.bin"]; !bytes.Equal(got, manifestBody) {
		t.Errorf("manifest artifact = %q, want %q", got, manifestBody)
	}
	if got := bundleEntries["artifacts/artifact-z.bin"]; !bytes.Equal(got, artifactBody) {
		t.Errorf("payload artifact = %q, want %q", got, artifactBody)
	}
	eventLines := strings.Split(strings.TrimSpace(string(bundleEntries["capture-events.ndjson"])), "\n")
	if len(eventLines) != 2 || !strings.Contains(eventLines[0], `"id":"event-a"`) || !strings.Contains(eventLines[1], `"id":"event-z"`) {
		t.Errorf("events are not sorted deterministically: %q", eventLines)
	}

	indexBody, err := os.ReadFile(filepath.Join(firstDir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index historyExportIndex
	if err := json.Unmarshal(indexBody, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Captures) != 2 || index.Captures[0].CaptureID != "capture-a" || !index.Captures[0].Available || index.Captures[1].CaptureID != "capture-z" || index.Captures[1].Available {
		t.Fatalf("index captures = %#v", index.Captures)
	}
	verifyHistoryTestSums(t, firstDir)

	code, _, stderr = runExport(firstDir, true)
	if code != 0 {
		t.Fatalf("rerun in frozen output directory = %d, stderr = %s", code, stderr)
	}

	secondDir := filepath.Join(t.TempDir(), "second")
	code, _, stderr = runExport(secondDir, true)
	if code != 0 {
		t.Fatalf("export with allow-incomplete = %d, stderr = %s", code, stderr)
	}
	for _, name := range []string{"20260803T100000.000000000Z-capture-a.tar", "export-descriptor.json", "index.json", "SHA256SUMS"} {
		first, err := os.ReadFile(filepath.Join(firstDir, name))
		if err != nil {
			t.Fatal(err)
		}
		second, err := os.ReadFile(filepath.Join(secondDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("%s differs across equivalent exports", name)
		}
	}
}

func TestNewHistoryResumeIdempotencyKeyIsFreshAndBounded(t *testing.T) {
	first, err := newHistoryResumeIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newHistoryResumeIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("fresh resume keys unexpectedly match: %q", first)
	}
	for _, key := range []string{first, second} {
		if len(key) > 255 || !strings.HasPrefix(key, "history-resume-") {
			t.Fatalf("generated resume key = %q", key)
		}
	}
}

func TestHistoryExportRejectsUnsafeOutputAndConflictingSelection(t *testing.T) {
	for _, args := range [][]string{
		{"export", "--output", "out"},
		{"export", "--output", "out", "--all", "--capture-id", "capture-1"},
		{"export", "--output", "out", "--all", "--parallel", "33"},
		{"export", "--output", "out", "--all", "--retries", "11"},
	} {
		var stdout, stderr bytes.Buffer
		if got := runHistory(args, &stdout, &stderr); got != 2 {
			t.Fatalf("runHistory(%q) = %d, want 2; stderr = %s", args, got, stderr.String())
		}
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareHistoryExportDirectory(dir); err == nil || !strings.Contains(err.Error(), "not private") {
		t.Fatalf("prepare public directory error = %v", err)
	}
	privateDir := filepath.Join(t.TempDir(), "unmarked")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareHistoryExportDirectory(privateDir); err == nil || !strings.Contains(err.Error(), "not marked") {
		t.Fatalf("prepare unmarked directory error = %v", err)
	}
}

func TestHistoryResumeUsesFreshDefaultIdempotencyKeyAcrossInvocations(t *testing.T) {
	var mu sync.Mutex
	var requests []struct {
		NativeSessionID string `json:"native_session_id"`
		IdempotencyKey  string `json:"idempotency_key"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/projects/project-1/history/captures/capture-1/resume" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			NativeSessionID string `json:"native_session_id"`
			IdempotencyKey  string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resume-1","source_capture_id":"capture-1","source_native_session_id":"native-1","job_id":"job-2","state":"queued","created":true}`)
	}))
	defer server.Close()

	for _, extra := range [][]string{nil, nil, {"--idempotency-key", "explicit-retry-key"}, {"--idempotency-key", "explicit-retry-key"}} {
		var stdout, stderr bytes.Buffer
		args := []string{
			"resume", "--server", server.URL, "--token", "owner", "--project", "project-1",
			"--native-session", "native-1",
		}
		args = append(args, extra...)
		args = append(args, "capture-1")
		code := runHistory(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runHistory(resume) code = %d, stderr = %s", code, stderr.String())
		}
		if got, want := stdout.String(), "resume-1\tjob-2\tqueued\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	}
	if len(requests) != 4 {
		t.Fatalf("resume requests = %d, want 4", len(requests))
	}
	for _, request := range requests {
		if request.NativeSessionID != "native-1" {
			t.Fatalf("native session requests = %#v", requests)
		}
	}
	if requests[0].IdempotencyKey == "" || requests[1].IdempotencyKey == "" {
		t.Fatalf("default idempotency keys are empty: %#v", requests)
	}
	if requests[0].IdempotencyKey == requests[1].IdempotencyKey {
		t.Fatalf("independent resume invocations reused default idempotency key %q", requests[0].IdempotencyKey)
	}
	for _, request := range requests[:2] {
		if !strings.HasPrefix(request.IdempotencyKey, "history-resume-") || len(request.IdempotencyKey) > 255 {
			t.Errorf("default idempotency key = %q", request.IdempotencyKey)
		}
	}
	if requests[2].IdempotencyKey != "explicit-retry-key" || requests[3].IdempotencyKey != "explicit-retry-key" {
		t.Fatalf("explicit retry keys = %q, %q", requests[2].IdempotencyKey, requests[3].IdempotencyKey)
	}
}

func historyTestDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func readHistoryTestTar(t *testing.T, path string) (map[string][]byte, []*tar.Header) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	entries := make(map[string][]byte)
	var headers []*tar.Header
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		copyHeader := *header
		headers = append(headers, &copyHeader)
		entries[header.Name] = body
	}
	return entries, headers
}

func historyTestHeaderNames(headers []*tar.Header) []string {
	names := make([]string, len(headers))
	for i, header := range headers {
		names[i] = header.Name
	}
	return names
}

func verifyHistoryTestSums(t *testing.T, dir string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			t.Fatalf("invalid checksum line %q", line)
		}
		payload, err := os.ReadFile(filepath.Join(dir, parts[1]))
		if err != nil {
			t.Fatal(err)
		}
		if got := historyTestDigest(payload); got != parts[0] {
			t.Errorf("checksum for %s = %s, want %s", parts[1], got, parts[0])
		}
	}
}
