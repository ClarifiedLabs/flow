package main

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	flowclient "github.com/ClarifiedLabs/flow/internal/client"
)

const (
	historyExportMarkerName     = ".flow-history-export"
	historyExportDescriptorName = "export-descriptor.json"
	historyExportMarker         = "flow-history-export-v1\n"
	historyExportFormat         = "flow-history-export"
)

var historyCaptureStates = map[string]bool{
	"reserved": true, "running": true, "quiescing": true, "sealed": true,
	"uploading": true, "complete": true, "blocked": true, "lost": true, "waived": true,
}

func runHistory(args []string, stdout, stderr io.Writer) int {
	return runHistoryWithGlobalOptions(args, globalOptions{}, stdout, stderr)
}

func runHistoryWithGlobalOptions(args []string, options globalOptions, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: flow history {list|export|resume} [flags]")
		return 2
	}
	switch args[0] {
	case "list":
		return runHistoryList(options.withConfig(args[1:]), stdout, stderr)
	case "export":
		return runHistoryExport(options.withConfig(args[1:]), stdout, stderr)
	case "resume":
		return runHistoryResume(options.withConfig(args[1:]), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown history subcommand: %s\n", args[0])
		return 2
	}
}

type historyBoolFlag struct {
	set   bool
	value bool
}

func (f *historyBoolFlag) String() string {
	if f == nil || !f.set {
		return ""
	}
	return strconv.FormatBool(f.value)
}

func (f *historyBoolFlag) Set(raw string) error {
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("must be true or false")
	}
	f.set, f.value = true, value
	return nil
}

func (*historyBoolFlag) IsBoolFlag() bool { return true }

type historyFilterFlags struct {
	taskIDs    stringSliceFlag
	jobIDs     stringSliceFlag
	sessionIDs stringSliceFlag
	captureIDs stringSliceFlag
	states     stringSliceFlag
	since      string
	until      string
	resumable  historyBoolFlag
}

func (f *historyFilterFlags) add(flags *flag.FlagSet) {
	flags.Var(&f.taskIDs, "task-id", "task id filter (repeatable)")
	flags.Var(&f.jobIDs, "job-id", "job id filter (repeatable)")
	flags.Var(&f.sessionIDs, "session-id", "session id filter (repeatable)")
	flags.Var(&f.captureIDs, "capture-id", "capture id filter (repeatable)")
	flags.Var(&f.states, "state", "capture state filter (repeatable)")
	flags.StringVar(&f.since, "since", "", "include captures reserved at or after this RFC3339 time")
	flags.StringVar(&f.until, "until", "", "include captures reserved before this RFC3339 time")
	flags.Var(&f.resumable, "resumable", "filter by resumability (true or false)")
}

func (f *historyFilterFlags) clientFilter(limit int) (flowclient.HistoryCaptureFilter, error) {
	repeated := []struct {
		name   string
		values []string
	}{
		{name: "task-id", values: f.taskIDs.Values},
		{name: "job-id", values: f.jobIDs.Values},
		{name: "session-id", values: f.sessionIDs.Values},
		{name: "capture-id", values: f.captureIDs.Values},
		{name: "state", values: f.states.Values},
	}
	for _, filter := range repeated {
		if len(filter.values) > 50 {
			return flowclient.HistoryCaptureFilter{}, fmt.Errorf("--%s may be specified at most 50 times", filter.name)
		}
	}
	for _, state := range f.states.Values {
		if !historyCaptureStates[state] {
			return flowclient.HistoryCaptureFilter{}, fmt.Errorf("invalid history capture state: %s", state)
		}
	}
	if limit < 1 || limit > 200 {
		return flowclient.HistoryCaptureFilter{}, errors.New("limit must be an integer between 1 and 200")
	}
	filter := flowclient.HistoryCaptureFilter{
		TaskIDs: append([]string(nil), f.taskIDs.Values...), JobIDs: append([]string(nil), f.jobIDs.Values...),
		SessionIDs: append([]string(nil), f.sessionIDs.Values...), CaptureIDs: append([]string(nil), f.captureIDs.Values...),
		States: append([]string(nil), f.states.Values...), Limit: limit,
	}
	var err error
	if filter.Since, err = parseHistoryCLIClock("since", f.since); err != nil {
		return flowclient.HistoryCaptureFilter{}, err
	}
	if filter.Until, err = parseHistoryCLIClock("until", f.until); err != nil {
		return flowclient.HistoryCaptureFilter{}, err
	}
	if filter.Since != nil && filter.Until != nil && !filter.Until.After(*filter.Since) {
		return flowclient.HistoryCaptureFilter{}, errors.New("until must be after since")
	}
	if f.resumable.set {
		value := f.resumable.value
		filter.Resumable = &value
	}
	return filter, nil
}

func (f *historyFilterFlags) hasSelector() bool {
	return len(f.taskIDs.Values) > 0 || len(f.jobIDs.Values) > 0 || len(f.sessionIDs.Values) > 0 ||
		len(f.captureIDs.Values) > 0 || len(f.states.Values) > 0 || strings.TrimSpace(f.since) != "" ||
		strings.TrimSpace(f.until) != "" || f.resumable.set
}

func parseHistoryCLIClock(name, raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be RFC3339", name)
	}
	value = value.UTC()
	return &value, nil
}

func runHistoryList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("history list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var filters historyFilterFlags
	filters.add(flags)
	format := "table"
	limit := 50
	flags.IntVar(&limit, "limit", 50, "maximum captures to return (1-200)")
	flags.StringVar(&format, "format", "table", "output format: table or json")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "history list does not accept positional arguments")
		return 2
	}
	if format != "table" && format != "json" {
		fmt.Fprintln(stderr, "--format must be table or json")
		return 2
	}
	filter, err := filters.clientFilter(limit)
	if err != nil {
		fmt.Fprintf(stderr, "invalid history filter: %v\n", err)
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	response, err := client.ListHistoryCaptures(context.Background(), filter)
	if err != nil {
		fmt.Fprintf(stderr, "list history captures: %v\n", err)
		return 1
	}
	if format == "json" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(response); err != nil {
			fmt.Fprintf(stderr, "write history list: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "CAPTURE\tSTATE\tTASK\tJOB\tSESSION\tROLE\tRESUMABLE\tRESERVED")
	for _, capture := range response.Captures {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%t\t%s\n",
			capture.ID, capture.State, historyTableValue(capture.TaskID), historyTableValue(capture.JobID),
			historyTableValue(capture.SessionID), historyTableValue(capture.Role), capture.Resumable, capture.ReservedAt)
	}
	return 0
}

func historyTableValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

type historyExportDescriptor struct {
	Format        string                 `json:"format"`
	SchemaVersion int                    `json:"schema_version"`
	ServerURL     string                 `json:"server_url"`
	Project       string                 `json:"project"`
	Selection     historyExportSelection `json:"selection"`
	SnapshotUntil string                 `json:"snapshot_until"`
}

type historyExportSelection struct {
	All        bool     `json:"all"`
	TaskIDs    []string `json:"task_ids,omitempty"`
	JobIDs     []string `json:"job_ids,omitempty"`
	SessionIDs []string `json:"session_ids,omitempty"`
	CaptureIDs []string `json:"capture_ids,omitempty"`
	States     []string `json:"states,omitempty"`
	Since      string   `json:"since,omitempty"`
	Until      string   `json:"until,omitempty"`
	Resumable  *bool    `json:"resumable,omitempty"`
}

func historySelection(all bool, filter flowclient.HistoryCaptureFilter) historyExportSelection {
	selection := historyExportSelection{
		All: all, TaskIDs: filter.TaskIDs, JobIDs: filter.JobIDs, SessionIDs: filter.SessionIDs,
		CaptureIDs: filter.CaptureIDs, States: filter.States, Resumable: filter.Resumable,
	}
	if filter.Since != nil {
		selection.Since = filter.Since.UTC().Format(time.RFC3339Nano)
	}
	if filter.Until != nil {
		selection.Until = filter.Until.UTC().Format(time.RFC3339Nano)
	}
	return selection
}

type historyExportIndex struct {
	Format        string                    `json:"format"`
	SchemaVersion int                       `json:"schema_version"`
	Captures      []historyExportIndexEntry `json:"captures"`
}

type historyExportIndexEntry struct {
	CaptureID  string `json:"capture_id"`
	State      string `json:"state"`
	Available  bool   `json:"available"`
	Bundle     string `json:"bundle,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	StoredSize int64  `json:"stored_size,omitempty"`
	Error      string `json:"error,omitempty"`
}

type historyExportResult struct {
	entry historyExportIndexEntry
	err   error
}

func runHistoryExport(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("history export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var filters historyFilterFlags
	filters.add(flags)
	var outputDir string
	var all, allowIncomplete bool
	parallel, retries := 4, 2
	flags.StringVar(&outputDir, "output", "", "private export output directory")
	flags.BoolVar(&all, "all", false, "export all captures")
	flags.BoolVar(&allowIncomplete, "allow-incomplete", false, "succeed when selected captures are not complete")
	flags.IntVar(&parallel, "parallel", 4, "maximum concurrent capture exports (1-32)")
	flags.IntVar(&retries, "retries", 2, "artifact download retries (0-10)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "history export does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(outputDir) == "" {
		fmt.Fprintln(stderr, "--output is required")
		return 2
	}
	if all == filters.hasSelector() {
		fmt.Fprintln(stderr, "history export requires either --all or at least one selector, but not both")
		return 2
	}
	if parallel < 1 || parallel > 32 {
		fmt.Fprintln(stderr, "--parallel must be between 1 and 32")
		return 2
	}
	if retries < 0 || retries > 10 {
		fmt.Fprintln(stderr, "--retries must be between 0 and 10")
		return 2
	}
	filter, err := filters.clientFilter(200)
	if err != nil {
		fmt.Fprintf(stderr, "invalid history selector: %v\n", err)
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	if err := prepareHistoryExportDirectory(outputDir); err != nil {
		fmt.Fprintf(stderr, "prepare history export directory: %v\n", err)
		return 1
	}
	selection := historySelection(all, filter)
	descriptorPath := filepath.Join(outputDir, historyExportDescriptorName)
	descriptor, descriptorExists, err := loadHistoryExportDescriptor(descriptorPath)
	if err != nil {
		fmt.Fprintf(stderr, "read history export descriptor: %v\n", err)
		return 1
	}
	serverURL := client.URLForPath("")
	if descriptorExists {
		candidate := historyExportDescriptor{Format: historyExportFormat, SchemaVersion: 1, ServerURL: serverURL, Project: client.ProjectRef(), Selection: selection}
		if descriptor.Format != candidate.Format || descriptor.SchemaVersion != candidate.SchemaVersion ||
			descriptor.ServerURL != candidate.ServerURL || descriptor.Project != candidate.Project ||
			!historyExportSelectionsEqual(descriptor.Selection, candidate.Selection) {
			fmt.Fprintln(stderr, "existing history export descriptor does not match this server, project, or selection")
			return 1
		}
		frozenUntil, parseErr := time.Parse(time.RFC3339Nano, descriptor.SnapshotUntil)
		if parseErr != nil {
			fmt.Fprintf(stderr, "invalid history export descriptor snapshot: %v\n", parseErr)
			return 1
		}
		filter.Until = &frozenUntil
	}
	captures, snapshotUntil, err := listAllHistoryCaptures(context.Background(), client, filter)
	if err != nil {
		fmt.Fprintf(stderr, "list history captures: %v\n", err)
		return 1
	}
	if descriptorExists {
		if snapshotUntil != descriptor.SnapshotUntil {
			fmt.Fprintln(stderr, "history export snapshot differs from the existing descriptor")
			return 1
		}
	} else {
		descriptor = historyExportDescriptor{
			Format: historyExportFormat, SchemaVersion: 1, ServerURL: serverURL,
			Project: client.ProjectRef(), Selection: selection, SnapshotUntil: snapshotUntil,
		}
		body, marshalErr := marshalHistoryJSON(descriptor)
		if marshalErr != nil {
			fmt.Fprintf(stderr, "encode history export descriptor: %v\n", marshalErr)
			return 1
		}
		if err := writeExclusiveOrMatchingPrivate(descriptorPath, body); err != nil {
			fmt.Fprintf(stderr, "write history export descriptor: %v\n", err)
			return 1
		}
	}

	results := make([]historyExportResult, len(captures))
	semaphore := make(chan struct{}, parallel)
	var group sync.WaitGroup
	for i, capture := range captures {
		if capture.State != "complete" {
			err := fmt.Errorf("capture state is %s", capture.State)
			results[i] = historyExportResult{entry: historyExportIndexEntry{
				CaptureID: capture.ID, State: capture.State, Available: false, Error: err.Error(),
			}, err: err}
			continue
		}
		group.Add(1)
		go func(index int, value contract.HistoryCapture) {
			defer group.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			results[index] = exportHistoryCapture(context.Background(), client, outputDir, value, retries)
		}(i, capture)
	}
	group.Wait()

	index := historyExportIndex{Format: historyExportFormat, SchemaVersion: 1, Captures: make([]historyExportIndexEntry, len(results))}
	checksums := make(map[string]string, len(results)+2)
	descriptorBody, err := os.ReadFile(descriptorPath)
	if err != nil {
		fmt.Fprintf(stderr, "read history export descriptor: %v\n", err)
		return 1
	}
	descriptorDigest := sha256.Sum256(descriptorBody)
	checksums[historyExportDescriptorName] = hex.EncodeToString(descriptorDigest[:])
	hardFailure, incomplete := false, false
	for i, result := range results {
		index.Captures[i] = result.entry
		if result.entry.Available {
			checksums[result.entry.Bundle] = result.entry.SHA256
			continue
		}
		if captures[i].State != "complete" {
			incomplete = true
		} else {
			hardFailure = true
		}
		fmt.Fprintf(stderr, "export history capture %s: %v\n", captures[i].ID, result.err)
	}
	indexBody, err := marshalHistoryJSON(index)
	if err != nil {
		fmt.Fprintf(stderr, "encode history export index: %v\n", err)
		return 1
	}
	if err := writeAtomicPrivate(filepath.Join(outputDir, "index.json"), indexBody); err != nil {
		fmt.Fprintf(stderr, "write history export index: %v\n", err)
		return 1
	}
	indexDigest := sha256.Sum256(indexBody)
	checksums["index.json"] = hex.EncodeToString(indexDigest[:])
	checksumBody := historyChecksumFile(checksums)
	if err := writeAtomicPrivate(filepath.Join(outputDir, "SHA256SUMS"), checksumBody); err != nil {
		fmt.Fprintf(stderr, "write history export checksums: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%d captures\n", outputDir, len(captures))
	if hardFailure || (incomplete && !allowIncomplete) {
		return 1
	}
	return 0
}

func listAllHistoryCaptures(ctx context.Context, client *flowclient.Client, filter flowclient.HistoryCaptureFilter) ([]contract.HistoryCapture, string, error) {
	seenCaptures := make(map[string]bool)
	seenCursors := make(map[string]bool)
	var captures []contract.HistoryCapture
	var snapshotUntil string
	for {
		response, err := client.ListHistoryCaptures(ctx, filter)
		if err != nil {
			return nil, "", err
		}
		if snapshotUntil == "" {
			snapshotUntil = response.SnapshotUntil
		} else if response.SnapshotUntil != snapshotUntil {
			return nil, "", errors.New("history list snapshot changed between pages")
		}
		for _, capture := range response.Captures {
			if !seenCaptures[capture.ID] {
				seenCaptures[capture.ID] = true
				captures = append(captures, capture)
			}
		}
		if response.NextCursor == "" {
			break
		}
		if seenCursors[response.NextCursor] {
			return nil, "", errors.New("history list returned a repeated cursor")
		}
		seenCursors[response.NextCursor] = true
		filter.Cursor = response.NextCursor
	}
	sort.Slice(captures, func(i, j int) bool { return captures[i].ID < captures[j].ID })
	if _, err := time.Parse(time.RFC3339Nano, snapshotUntil); err != nil {
		return nil, "", fmt.Errorf("history list returned invalid snapshot_until: %w", err)
	}
	return captures, snapshotUntil, nil
}

func historyExportSelectionsEqual(left, right historyExportSelection) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func loadHistoryExportDescriptor(path string) (historyExportDescriptor, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return historyExportDescriptor{}, false, nil
	}
	if err != nil {
		return historyExportDescriptor{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return historyExportDescriptor{}, false, errors.New("export descriptor is not a private regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return historyExportDescriptor{}, false, err
	}
	var descriptor historyExportDescriptor
	if err := json.Unmarshal(body, &descriptor); err != nil {
		return historyExportDescriptor{}, false, err
	}
	return descriptor, true, nil
}

func prepareHistoryExportDirectory(path string) error {
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		return createHistoryExportMarker(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("output path must be a directory, not a symlink or file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("output directory permissions %04o are not private", info.Mode().Perm())
	}
	markerPath := filepath.Join(path, historyExportMarkerName)
	markerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return errors.New("existing output directory is not marked as a Flow history export")
	}
	if !markerInfo.Mode().IsRegular() || markerInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("history export marker is not a private regular file")
	}
	body, err := os.ReadFile(markerPath)
	if err != nil {
		return err
	}
	if string(body) != historyExportMarker {
		return errors.New("history export marker has unrecognized contents")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == historyExportMarkerName || name == historyExportDescriptorName || name == "index.json" || name == "SHA256SUMS" {
			continue
		}
		if entry.Type().IsRegular() && strings.HasSuffix(name, ".tar") && !strings.HasPrefix(name, ".") {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			if info.Mode().Perm()&0o077 == 0 {
				continue
			}
		}
		return fmt.Errorf("existing output directory contains unrelated or unsafe entry %q", name)
	}
	return nil
}

func createHistoryExportMarker(dir string) error {
	path := filepath.Join(dir, historyExportMarkerName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(file, historyExportMarker); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func exportHistoryCapture(ctx context.Context, client *flowclient.Client, outputDir string, capture contract.HistoryCapture, retries int) historyExportResult {
	entry := historyExportIndexEntry{CaptureID: capture.ID, State: capture.State}
	if !safeHistoryExportID(capture.ID) {
		entry.Error = "capture id is unsafe for export"
		return historyExportResult{entry: entry, err: errors.New(entry.Error)}
	}
	detail, err := client.GetHistoryCapture(ctx, capture.ID)
	if err != nil {
		entry.Error = err.Error()
		return historyExportResult{entry: entry, err: err}
	}
	if detail.Capture.ID != capture.ID || detail.Capture.State != "complete" {
		err := errors.New("capture changed or detail response did not match the selected capture")
		entry.Error = err.Error()
		return historyExportResult{entry: entry, err: err}
	}
	eventsResponse, err := client.ListHistoryCaptureEvents(ctx, capture.ID)
	if err != nil {
		entry.Error = err.Error()
		return historyExportResult{entry: entry, err: err}
	}
	sort.Slice(detail.Artifacts, func(i, j int) bool { return detail.Artifacts[i].ID < detail.Artifacts[j].ID })
	sort.Slice(eventsResponse.Events, func(i, j int) bool {
		if eventsResponse.Events[i].OccurredAt == eventsResponse.Events[j].OccurredAt {
			return eventsResponse.Events[i].ID < eventsResponse.Events[j].ID
		}
		return eventsResponse.Events[i].OccurredAt < eventsResponse.Events[j].OccurredAt
	})
	workDir, err := os.MkdirTemp(outputDir, ".capture-"+capture.ID+"-")
	if err != nil {
		entry.Error = err.Error()
		return historyExportResult{entry: entry, err: err}
	}
	defer os.RemoveAll(workDir)

	manifest, err := findHistoryManifest(detail.Artifacts)
	if err != nil {
		entry.Error = err.Error()
		return historyExportResult{entry: entry, err: err}
	}
	manifestPath := filepath.Join(workDir, "canonical-manifest")
	if err := downloadHistoryFile(retries, manifestPath, manifest.SHA256, manifest.StoredSize, func(dst io.Writer) error {
		return client.DownloadHistoryManifest(ctx, capture.ID, dst)
	}); err != nil {
		entry.Error = fmt.Sprintf("download canonical manifest: %v", err)
		return historyExportResult{entry: entry, err: errors.New(entry.Error)}
	}

	artifactPaths := make(map[string]string)
	for i, artifact := range detail.Artifacts {
		if artifact.PublicationState != "committed" {
			continue
		}
		if !safeHistoryExportID(artifact.ID) {
			err := fmt.Errorf("artifact id %q is unsafe for export", artifact.ID)
			entry.Error = err.Error()
			return historyExportResult{entry: entry, err: err}
		}
		path := filepath.Join(workDir, fmt.Sprintf("artifact-%06d", i))
		artifact := artifact
		if err := downloadHistoryFile(retries, path, artifact.SHA256, artifact.StoredSize, func(dst io.Writer) error {
			return client.DownloadHistoryArtifact(ctx, capture.ID, artifact.ID, dst)
		}); err != nil {
			entry.Error = fmt.Sprintf("download artifact %s: %v", artifact.ID, err)
			return historyExportResult{entry: entry, err: errors.New(entry.Error)}
		}
		artifactPaths[artifact.ID] = path
	}
	bundleName, err := historyExportBundleName(capture)
	if err != nil {
		entry.Error = err.Error()
		return historyExportResult{entry: entry, err: err}
	}
	bundlePath := filepath.Join(outputDir, bundleName)
	digest, size, err := writeHistoryBundle(bundlePath, detail, eventsResponse.Events, manifestPath, artifactPaths)
	if err != nil {
		entry.Error = err.Error()
		return historyExportResult{entry: entry, err: err}
	}
	entry.Available, entry.Bundle, entry.SHA256, entry.StoredSize = true, bundleName, digest, size
	return historyExportResult{entry: entry}
}

func historyExportBundleName(capture contract.HistoryCapture) (string, error) {
	reservedAt, err := time.Parse(time.RFC3339Nano, capture.ReservedAt)
	if err != nil {
		return "", fmt.Errorf("capture %s has invalid reserved_at: %w", capture.ID, err)
	}
	stamp := reservedAt.UTC().Format("20060102T150405.000000000Z")
	return stamp + "-" + capture.ID + ".tar", nil
}

func safeHistoryExportID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func findHistoryManifest(artifacts []contract.HistoryArtifact) (contract.HistoryArtifact, error) {
	var found *contract.HistoryArtifact
	for i := range artifacts {
		artifact := &artifacts[i]
		if artifact.Kind != "manifest" || artifact.Phase != "final" || artifact.PublicationState != "committed" {
			continue
		}
		if found != nil {
			return contract.HistoryArtifact{}, errors.New("capture has more than one committed final manifest")
		}
		found = artifact
	}
	if found == nil {
		return contract.HistoryArtifact{}, errors.New("capture has no committed final manifest")
	}
	return *found, nil
}

type historyCountingWriter struct{ count int64 }

func (w *historyCountingWriter) Write(body []byte) (int, error) {
	w.count += int64(len(body))
	return len(body), nil
}

func downloadHistoryFile(retries int, path, wantDigest string, wantSize int64, download func(io.Writer) error) error {
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		digest := sha256.New()
		count := &historyCountingWriter{}
		writer := io.MultiWriter(file, digest, count)
		err = download(writer)
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = verifyHistoryDownload(digest, count.count, wantDigest, wantSize)
		}
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func verifyHistoryDownload(digest hash.Hash, size int64, wantDigest string, wantSize int64) error {
	gotDigest := hex.EncodeToString(digest.Sum(nil))
	if !strings.EqualFold(gotDigest, strings.TrimSpace(wantDigest)) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", gotDigest, wantDigest)
	}
	if size != wantSize {
		return fmt.Errorf("stored length mismatch: got %d, want %d", size, wantSize)
	}
	return nil
}

func writeHistoryBundle(path string, detail contract.HistoryCaptureResponse, events []contract.HistoryCaptureEvent, manifestPath string, artifactPaths map[string]string) (string, int64, error) {
	captureBody, err := marshalHistoryJSON(detail.Capture)
	if err != nil {
		return "", 0, err
	}
	artifactsBody, err := marshalHistoryJSON(detail.Artifacts)
	if err != nil {
		return "", 0, err
	}
	var eventsBody []byte
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			return "", 0, err
		}
		eventsBody = append(eventsBody, line...)
		eventsBody = append(eventsBody, '\n')
	}
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", 0, err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".bundle-*")
	if err != nil {
		return "", 0, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return "", 0, err
	}
	digest := sha256.New()
	count := &historyCountingWriter{}
	tarWriter := tar.NewWriter(io.MultiWriter(temp, digest, count))
	writeBytes := func(name string, body []byte) error {
		header := deterministicHistoryTarHeader(name, int64(len(body)))
		if err := tarWriter.WriteHeader(&header); err != nil {
			return err
		}
		_, err := tarWriter.Write(body)
		return err
	}
	err = writeBytes("capture.json", captureBody)
	if err == nil {
		err = writeBytes("capture-events.ndjson", eventsBody)
	}
	if err == nil {
		err = writeBytes("artifacts.json", artifactsBody)
	}
	if err == nil {
		err = writeBytes("canonical-manifest.json", manifestBody)
	}
	artifactIDs := make([]string, 0, len(artifactPaths))
	for id := range artifactPaths {
		artifactIDs = append(artifactIDs, id)
	}
	sort.Strings(artifactIDs)
	for _, id := range artifactIDs {
		if err != nil {
			break
		}
		info, statErr := os.Stat(artifactPaths[id])
		if statErr != nil {
			err = statErr
			break
		}
		header := deterministicHistoryTarHeader("artifacts/"+id+".bin", info.Size())
		if err = tarWriter.WriteHeader(&header); err != nil {
			break
		}
		file, openErr := os.Open(artifactPaths[id])
		if openErr != nil {
			err = openErr
			break
		}
		_, copyErr := io.Copy(tarWriter, file)
		closeErr := file.Close()
		if copyErr != nil {
			err = copyErr
		} else if closeErr != nil {
			err = closeErr
		}
	}
	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, err
	}
	if err := installPrivateTemp(tempPath, path); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), count.count, nil
}

func deterministicHistoryTarHeader(name string, size int64) tar.Header {
	epoch := time.Unix(0, 0).UTC()
	return tar.Header{
		Name: name, Mode: 0o600, Size: size, ModTime: epoch,
		Typeflag: tar.TypeReg, Format: tar.FormatUSTAR,
	}
}

func marshalHistoryJSON(value any) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func writeAtomicPrivate(path string, body []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err = temp.Write(body); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return replaceManagedPrivateTemp(tempPath, path)
}

func writeExclusiveOrMatchingPrivate(path string, body []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".write-once-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err = temp.Write(body); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return installPrivateTemp(tempPath, path)
}

func installPrivateTemp(tempPath, targetPath string) error {
	if err := os.Link(tempPath, targetPath); err == nil {
		return os.Remove(tempPath)
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(targetPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing to reuse non-private generated file %s", targetPath)
	}
	equal, err := historyFilesEqual(tempPath, targetPath)
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("refusing to overwrite differing generated file %s", targetPath)
	}
	return os.Remove(tempPath)
}

func replaceManagedPrivateTemp(tempPath, targetPath string) error {
	if info, err := os.Lstat(targetPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("refusing to replace non-private generated file %s", targetPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tempPath, targetPath)
}

func historyFilesEqual(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false, err
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false, err
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}
	digestFile := func(path string) ([sha256.Size]byte, error) {
		file, err := os.Open(path)
		if err != nil {
			return [sha256.Size]byte{}, err
		}
		defer file.Close()
		digest := sha256.New()
		if _, err := io.Copy(digest, file); err != nil {
			return [sha256.Size]byte{}, err
		}
		var result [sha256.Size]byte
		copy(result[:], digest.Sum(nil))
		return result, nil
	}
	leftDigest, err := digestFile(left)
	if err != nil {
		return false, err
	}
	rightDigest, err := digestFile(right)
	return err == nil && leftDigest == rightDigest, err
}

func historyChecksumFile(checksums map[string]string) []byte {
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	var body strings.Builder
	for _, name := range names {
		fmt.Fprintf(&body, "%s  %s\n", checksums[name], name)
	}
	return []byte(body.String())
}

func runHistoryResume(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("history resume", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiFlags := addAPIFlags(flags)
	var nativeSession, idempotencyKey string
	flags.StringVar(&nativeSession, "native-session", "", "native harness session id within the capture")
	flags.StringVar(&idempotencyKey, "idempotency-key", "", "idempotency key (fresh random key when omitted; reuse explicitly for uncertain retries)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" {
		fmt.Fprintln(stderr, "usage: flow history resume [flags] CAPTURE_ID")
		return 2
	}
	captureID := strings.TrimSpace(flags.Arg(0))
	nativeSession = strings.TrimSpace(nativeSession)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(nativeSession) > 255 || len(idempotencyKey) > 255 {
		fmt.Fprintln(stderr, "history resume metadata must not exceed 255 bytes")
		return 2
	}
	client, err := newAPIClient(apiFlags)
	if err != nil {
		fmt.Fprintf(stderr, "create client: %v\n", err)
		return 1
	}
	if idempotencyKey == "" {
		idempotencyKey, err = newHistoryResumeIdempotencyKey()
		if err != nil {
			fmt.Fprintf(stderr, "generate history resume idempotency key: %v\n", err)
			return 1
		}
	}
	response, err := client.ResumeHistoryCapture(context.Background(), captureID, contract.ResumeHistoryCaptureRequest{
		NativeSessionID: nativeSession, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		fmt.Fprintf(stderr, "resume history capture: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\t%s\t%s\n", response.ID, response.JobID, response.State)
	return 0
}

func newHistoryResumeIdempotencyKey() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "history-resume-" + hex.EncodeToString(random[:]), nil
}
