package historycapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/historyarchive"
)

func TestStageIsDeterministicAndEmptySealIsCanonical(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, nil, false)
	first := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	second := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	for index, outbox := range []*Outbox{first, second} {
		capture := testCapture(fmt.Sprintf("capture-%d", index), false)
		if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
			t.Fatal(err)
		}
		entry, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "succeeded", ExitCode: intPointer(0)})
		if err != nil {
			t.Fatal(err)
		}
		if entry.TranscriptSeal == nil || entry.TranscriptSeal.FinalEpoch != -1 || entry.TranscriptSeal.SegmentCount != 0 || entry.TranscriptSeal.LogicalLength != 0 || entry.TranscriptSeal.SHA256 != emptySHA256 {
			t.Fatalf("seal = %+v", entry.TranscriptSeal)
		}
		if len(entry.Artifacts) != 1 || entry.Artifacts[0].Publish.Kind != "workspace_snapshot" {
			t.Fatalf("artifacts = %+v", entry.Artifacts)
		}
		if entry.HarnessMembers != nil {
			t.Fatal("non-Harness capture retained Harness metadata")
		}
		if _, err := os.Stat(filepath.Join(outbox.root, capture.ID, harnessFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-Harness archive stat error = %v", err)
		}
	}
	firstEntry := mustLoad(t, first, "capture-0")
	secondEntry := mustLoad(t, second, "capture-1")
	if !reflect.DeepEqual(firstEntry.Artifacts[0].Publish, secondEntry.Artifacts[0].Publish) || firstEntry.Artifacts[0].SHA256 != secondEntry.Artifacts[0].SHA256 {
		t.Fatalf("deterministic records differ:\n%+v\n%+v", firstEntry.Artifacts, secondEntry.Artifacts)
	}
	firstBytes := mustRead(t, filepath.Join(first.root, "capture-0", workspaceFileName))
	secondBytes := mustRead(t, filepath.Join(second.root, "capture-1", workspaceFileName))
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("workspace archives differ for identical source state")
	}
	assertPrivateTree(t, first.root)
}

func TestRecordReservationRecoversIncompleteAtomicInitialization(t *testing.T) {
	ctx := context.Background()
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	incomplete, err := os.MkdirTemp(outbox.root, reservationTempDirPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(incomplete, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomplete, stateFileName), []byte("partial private state"), 0600); err != nil {
		t.Fatal(err)
	}

	capture := testCapture("atomic-reservation", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: testSources(t, nil, false)}); err != nil {
		t.Fatalf("record reservation after interrupted initialization: %v", err)
	}
	if _, err := os.Stat(incomplete); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete reservation directory stat error = %v", err)
	}
	pending, err := outbox.ListPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Capture.ID != capture.ID {
		t.Fatalf("pending reservations = %+v", pending)
	}
}

func TestRecordReservationRequiresSensitiveDataKey(t *testing.T) {
	ctx := context.Background()
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("sensitive-key-required", false)
	_, err := outbox.RecordReservation(ctx, Reservation{
		Response:        testReservation(capture),
		Sources:         testSources(t, []byte("transcript"), false),
		SensitiveValues: [][]byte{[]byte("attempt-only-secret")},
	})
	if !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("RecordReservation() error = %v, want missing-key rejection", err)
	}
}

func TestStageAfterRestartUsesEncryptedReservationSensitiveValues(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "outbox")
	key := []byte("durable-sensitive-data-key")
	secret := []byte("ATTEMPT-ONLY-SECRET-42")
	sources := testSources(t, append([]byte("prefix-"), secret...), false)
	capture := testCapture("sensitive-restart", false)
	options := testOptions(root, 4)
	options.SensitiveDataKey = key
	outbox, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := outbox.RecordReservation(ctx, Reservation{
		Response:        testReservation(capture),
		Sources:         sources,
		SensitiveValues: [][]byte{nil, secret, secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.SensitiveValuesCiphertext == nil || len(entry.SensitiveValuesCiphertext.Nonce) != sensitiveValuesNonceBytes {
		t.Fatalf("encrypted sensitive values = %+v", entry.SensitiveValuesCiphertext)
	}
	statePath := filepath.Join(root, capture.ID, stateFileName)
	state := mustRead(t, statePath)
	if bytes.Contains(state, secret) {
		t.Fatalf("state contains plaintext sensitive value: %s", state)
	}
	if !bytes.Contains(state, []byte(`"sensitive_values_ciphertext"`)) {
		t.Fatalf("state has no encrypted sensitive values: %s", state)
	}
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources, SensitiveValues: [][]byte{secret}}); err != nil {
		t.Fatalf("same normalized reservation replay: %v", err)
	}
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources, SensitiveValues: [][]byte{[]byte("different-secret")}}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("changed reservation replay error = %v, want rejection", err)
	}

	restarted, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Stage(ctx, capture.ID, Final{Verdict: "failed"}); !errors.Is(err, historyarchive.ErrSensitiveContent) {
		t.Fatalf("Stage() after restart error = %v, want sensitive-content rejection", err)
	}
}

func TestStageWithWrongSensitiveDataKeyFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "outbox")
	capture := testCapture("sensitive-wrong-key", false)
	options := testOptions(root, 4)
	options.SensitiveDataKey = []byte("correct-key")
	outbox, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.RecordReservation(ctx, Reservation{
		Response: testReservation(capture), Sources: testSources(t, []byte("clean transcript"), false),
		SensitiveValues: [][]byte{[]byte("attempt-secret")},
	}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, capture.ID, stateFileName)
	before := mustRead(t, statePath)
	options.SensitiveDataKey = []byte("wrong-key")
	restarted, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Stage(ctx, capture.ID, Final{Verdict: "failed"}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("Stage() error = %v, want authenticated-decryption failure", err)
	}
	if after := mustRead(t, statePath); !bytes.Equal(before, after) {
		t.Fatal("wrong-key staging mutated durable state")
	}
}

func TestSuccessfulStageAndCompletionScrubSensitiveCiphertext(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "outbox")
	key := []byte("scrub-sensitive-data-key")
	secret := []byte("attempt-secret-to-scrub")
	capture := testCapture("sensitive-scrub", false)
	options := testOptions(root, 4)
	options.SensitiveDataKey = key
	outbox, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	sources := testSources(t, []byte("clean transcript"), false)
	reservation := Reservation{Response: testReservation(capture), Sources: sources, SensitiveValues: [][]byte{secret}}
	if _, err := outbox.RecordReservation(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := restarted.Stage(ctx, capture.ID, Final{Verdict: "succeeded", ExitCode: intPointer(0)})
	if err != nil {
		t.Fatal(err)
	}
	if entry.SensitiveValuesCiphertext != nil {
		t.Fatal("staged entry retained sensitive value ciphertext")
	}
	statePath := filepath.Join(root, capture.ID, stateFileName)
	state := mustRead(t, statePath)
	if bytes.Contains(state, secret) || bytes.Contains(state, []byte(`"sensitive_values_ciphertext"`)) {
		t.Fatalf("staged state retained sensitive data: %s", state)
	}

	capture.State = "complete"
	capture.Version++
	reservation.Response = testReservation(capture)
	if _, err := restarted.RecordReservation(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	completed := mustLoad(t, restarted, capture.ID)
	if completed.Status != statusComplete || completed.SensitiveValuesCiphertext != nil {
		t.Fatalf("completed tombstone retained sensitive state: %+v", completed)
	}
	state = mustRead(t, statePath)
	if bytes.Contains(state, secret) || bytes.Contains(state, []byte(`"sensitive_values_ciphertext"`)) {
		t.Fatalf("completed state retained sensitive data: %s", state)
	}
}

func TestStageSegmentsTranscriptAtDeterministicIdentityBoundaries(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, []byte("abcdefghij"), false)
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("segments", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	entry, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "failed", ExitCode: intPointer(2), ErrorCode: "command_failed"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.TranscriptSeal.FinalEpoch != 0 || entry.TranscriptSeal.SegmentCount != 3 || entry.TranscriptSeal.LogicalLength != 10 || entry.TranscriptSeal.SHA256 != digest([]byte("abcdefghij")) {
		t.Fatalf("seal = %+v", entry.TranscriptSeal)
	}
	var segments []artifactRecord
	for _, artifact := range entry.Artifacts {
		if artifact.Segment != nil {
			segments = append(segments, artifact)
			if artifact.StoredSize > 4 || artifact.Publish.MediaType != "text/plain; charset=utf-8" || artifact.Segment.Encoding != "identity" {
				t.Fatalf("segment = %+v", artifact)
			}
		}
	}
	if len(segments) != 3 {
		t.Fatalf("segments = %d", len(segments))
	}
	for index, bounds := range [][2]int64{{0, 4}, {4, 8}, {8, 10}} {
		segment := segments[index]
		if segment.Segment.Sequence != int64(index) || segment.Segment.StartOffset != bounds[0] || segment.Segment.EndOffset != bounds[1] || segment.Publish.LogicalKey != fmt.Sprintf("transcript/%012d", index) {
			t.Fatalf("segment %d = %+v", index, segment)
		}
	}
}

func TestStageRejectsSensitiveTranscriptAcrossSegmentBoundary(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, []byte("abcTOKENxyz"), false)
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("sensitive-transcript", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{
		Response: testReservation(capture), Sources: sources,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "failed", SensitiveValues: [][]byte{[]byte("TOKEN")}}); !errors.Is(err, historyarchive.ErrSensitiveContent) {
		t.Fatalf("Stage() error = %v, want sensitive-content rejection", err)
	}
}

func TestStageRejectsSensitiveHarnessNativeState(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, []byte("transcript"), true)
	secret := []byte("SESSION-TOKEN-123")
	writeJSONFile(t, filepath.Join(sources.NativeSessionRoot, "children", "child", "secret.json"), map[string]any{"token": string(secret)})
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("sensitive-harness", true)
	if _, err := outbox.RecordReservation(ctx, Reservation{
		Response: testReservation(capture), Sources: sources,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "failed", SensitiveValues: [][]byte{secret}}); !errors.Is(err, historyarchive.ErrSensitiveContent) {
		t.Fatalf("Stage() error = %v, want sensitive-content rejection", err)
	}
}

func TestHarnessStageWritesAndInspectsExactlyOneNativeArchive(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, []byte("transcript"), true)
	// Flow reserves the capture before Harness creates state.json, so the native
	// root ID is discovered from the final native archive rather than guessed.
	sources.NativeSessionID = ""
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("harness", true)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	entry, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "succeeded", ExitCode: intPointer(0)})
	if err != nil {
		t.Fatal(err)
	}
	var harnessCount int
	for _, artifact := range entry.Artifacts {
		if artifact.Publish.Kind == "harness_root" {
			harnessCount++
			file, err := os.Open(filepath.Join(outbox.root, capture.ID, artifact.Path))
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := historyarchive.Inspect(ctx, file, outbox.options.ArchiveLimits)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			if inspection.Harness == nil || inspection.Harness.RootSessionID != "native-root" || inspection.Harness.HarnessBuild != "v0.4.5" || len(inspection.Harness.Members) != 2 {
				t.Fatalf("inspection = %+v", inspection.Harness)
			}
		}
	}
	if entry.Sources.NativeSessionID != "native-root" {
		t.Fatalf("discovered native session ID = %q", entry.Sources.NativeSessionID)
	}
	if harnessCount != 1 || entry.HarnessMembers == nil || len(entry.HarnessMembers.Members) != 2 {
		t.Fatalf("Harness artifacts=%d members=%+v", harnessCount, entry.HarnessMembers)
	}
	for _, member := range entry.HarnessMembers.Members {
		if member.ParseStatus != "parsed" || member.HarnessBuild != "v0.4.5" {
			t.Fatalf("member = %+v", member)
		}
	}
}

func TestLoadRejectsPathTamperingAndInsecureModes(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, []byte("transcript"), false)
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("tamper", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	entry, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "crashed", ErrorCode: "worker_crash"})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	entry.Artifacts[0].Path = filepath.Join("..", "..", filepath.Base(outside))
	statePath := filepath.Join(outbox.root, capture.ID, stateFileName)
	encoded, _ := json.Marshal(entry)
	if err := os.WriteFile(statePath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.ListPending(ctx); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("path tampering error = %v", err)
	}

	secure := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture = testCapture("mode", false)
	if _, err := secure.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(secure.root, capture.ID, stateFileName), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := secure.ListPending(ctx); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("mode tampering error = %v", err)
	}
}

func TestOutstandingCountAndByteLimits(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, nil, false)
	options := testOptions(filepath.Join(t.TempDir(), "outbox"), 4)
	options.MaxOutstandingEntries = 1
	outbox, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	first := testCapture("first", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(first), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	second := testCapture("second", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(second), Sources: sources}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("count error = %v", err)
	}

	options = testOptions(filepath.Join(t.TempDir(), "outbox"), 4)
	options.MaxOutstandingBytes = 1
	outbox, err = New(options)
	if err != nil {
		t.Fatal(err)
	}
	capture := testCapture("bytes", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "succeeded"}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("byte error = %v", err)
	}
}

func TestStageBoundsArchiveWritersByAggregateRemaining(t *testing.T) {
	ctx := context.Background()
	for _, harness := range []bool{false, true} {
		name := "workspace"
		if harness {
			name = "harness"
		}
		t.Run(name, func(t *testing.T) {
			sources := testSources(t, []byte("transcript"), harness)
			baseline := newTestOutbox(t, filepath.Join(t.TempDir(), "baseline"), 4)
			capture := testCapture("baseline", harness)
			if _, err := baseline.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
				t.Fatal(err)
			}
			entry, err := baseline.Stage(ctx, capture.ID, Final{Verdict: "succeeded"})
			if err != nil {
				t.Fatal(err)
			}
			var total int64
			for _, artifact := range entry.Artifacts {
				total += artifact.StoredSize
			}
			if total < 2 {
				t.Fatalf("baseline staged bytes = %d", total)
			}

			options := testOptions(filepath.Join(t.TempDir(), "bounded"), 4)
			options.MaxOutstandingBytes = total - 1
			bounded, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			capture = testCapture("bounded", harness)
			if _, err := bounded.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
				t.Fatal(err)
			}
			_, err = bounded.Stage(ctx, capture.ID, Final{Verdict: "succeeded"})
			if !errors.Is(err, ErrLimitExceeded) || !errors.Is(err, historyarchive.ErrLimitExceeded) {
				t.Fatalf("Stage() error = %v, want aggregate archive-writer limit", err)
			}
		})
	}
}

func TestStageIncludesOtherDurableArtifactsInWriterBound(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, []byte("transcript"), false)
	baseline := newTestOutbox(t, filepath.Join(t.TempDir(), "baseline"), 4)
	capture := testCapture("baseline", false)
	if _, err := baseline.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	entry, err := baseline.Stage(ctx, capture.ID, Final{Verdict: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	var captureBytes int64
	for _, artifact := range entry.Artifacts {
		captureBytes += artifact.StoredSize
	}

	options := testOptions(filepath.Join(t.TempDir(), "outbox"), 4)
	options.MaxOutstandingBytes = captureBytes*2 - 1
	outbox, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	first := testCapture("first-bytes", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(first), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Stage(ctx, first.ID, Final{Verdict: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	second := testCapture("second-bytes", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(second), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	_, err = outbox.Stage(ctx, second.ID, Final{Verdict: "succeeded"})
	if !errors.Is(err, ErrLimitExceeded) || !errors.Is(err, historyarchive.ErrLimitExceeded) {
		t.Fatalf("Stage() error = %v, want remaining aggregate archive-writer limit", err)
	}
}

func TestStageTranscriptEnforcesRunningBoundDuringGrowth(t *testing.T) {
	const initialSize = 512 << 10
	root := t.TempDir()
	source := filepath.Join(root, "transcript.txt")
	if err := os.WriteFile(source, bytes.Repeat([]byte("a"), initialSize), 0600); err != nil {
		t.Fatal(err)
	}
	entryDir := filepath.Join(root, "entry")
	if err := os.Mkdir(entryDir, 0700); err != nil {
		t.Fatal(err)
	}
	options := testOptions(filepath.Join(root, "outbox"), 1024)
	outbox, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	appended := make(chan error, 1)
	go func() {
		firstSegment := filepath.Join(entryDir, "transcript-000000000000.bin")
		for {
			if _, err := os.Stat(firstSegment); err == nil {
				file, openErr := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0600)
				if openErr == nil {
					_, openErr = file.Write(bytes.Repeat([]byte("b"), 4096))
					if closeErr := file.Close(); openErr == nil {
						openErr = closeErr
					}
				}
				appended <- openErr
				return
			}
		}
	}()
	_, _, err = outbox.stageTranscript(context.Background(), entryDir, source, nil, initialSize)
	if appendErr := <-appended; appendErr != nil {
		t.Fatal(appendErr)
	}
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("stageTranscript() error = %v, want running byte limit", err)
	}
}

func TestStageRemovesKnownCrashLeftPayloadTempsAcrossEntries(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, nil, false)
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	first := testCapture("first-temp", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(first), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.RecordFinal(ctx, first.ID, Final{Verdict: "failed"}); err != nil {
		t.Fatal(err)
	}
	entryDir := filepath.Join(outbox.root, first.ID)
	known := filepath.Join(entryDir, "."+workspaceFileName+".tmp-0123456789abcdef01234567")
	unknown := filepath.Join(entryDir, ".notes.tmp-0123456789abcdef01234567")
	if err := os.WriteFile(known, bytes.Repeat([]byte("x"), 1024), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	second := testCapture("second-temp", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(second), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Stage(ctx, second.ID, Final{Verdict: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(known); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("known stage temp stat error = %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unrelated temp was removed: %v", err)
	}
}

func TestPruneCompletedBoundsAgeAndCountWithoutTouchingPendingPayloads(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, []byte("pending transcript"), false)
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)

	pending := testCapture("pending", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(pending), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	pendingEntry, err := outbox.Stage(ctx, pending.ID, Final{Verdict: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	payloads := make(map[string][]byte, len(pendingEntry.Artifacts))
	for _, artifact := range pendingEntry.Artifacts {
		payloads[artifact.Path] = mustRead(t, filepath.Join(outbox.root, pending.ID, artifact.Path))
	}

	now := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	completed := []struct {
		id string
		at time.Time
	}{
		{id: "completed-old", at: now.Add(-48 * time.Hour)},
		{id: "completed-middle", at: now.Add(-3 * time.Hour)},
		{id: "completed-newer", at: now.Add(-2 * time.Hour)},
		{id: "completed-newest", at: now.Add(-time.Hour)},
	}
	for _, item := range completed {
		capture := testCapture(item.id, false)
		if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
			t.Fatal(err)
		}
		capture.State = "complete"
		capture.ExecutionVerdict = "succeeded"
		capture.Version++
		capture.CompletedAt = item.at.Format(time.RFC3339Nano)
		if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := outbox.PruneCompleted(ctx, CompletedRetention{MaxEntries: 2, CompletedBefore: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("PruneCompleted() removed %d entries, want 2", removed)
	}
	for _, id := range []string{"completed-old", "completed-middle"} {
		if _, err := outbox.Get(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(%q) error = %v, want not found", id, err)
		}
	}
	for _, id := range []string{"completed-newer", "completed-newest"} {
		entry, err := outbox.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Status != statusComplete || len(entry.Artifacts) != 0 || entry.UploadGrant != "" {
			t.Fatalf("retained tombstone %q is not scrubbed: %+v", id, entry)
		}
	}
	entry, err := outbox.Get(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Status != statusStaged {
		t.Fatalf("pending status = %q", entry.Status)
	}
	for path, want := range payloads {
		if got := mustRead(t, filepath.Join(outbox.root, pending.ID, path)); !bytes.Equal(got, want) {
			t.Fatalf("pending payload %q changed", path)
		}
	}
}

func TestRecordFinalConsoleRequiresCredentialAndSensitiveDataKey(t *testing.T) {
	ctx := context.Background()
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("terminal-key-required", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: testSources(t, nil, false)}); err != nil {
		t.Fatal(err)
	}
	withoutToken := &TerminalAction{Kind: TerminalConsoleExit, SessionID: "session-1", ExitCode: 7}
	if _, err := outbox.RecordFinal(ctx, capture.ID, Final{Verdict: "failed", ExitCode: intPointer(7), Terminal: withoutToken}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("missing-token RecordFinal() error = %v, want rejection", err)
	}
	withoutToken.SessionToken = "private-session-token"
	if _, err := outbox.RecordFinal(ctx, capture.ID, Final{Verdict: "failed", ExitCode: intPointer(7), Terminal: withoutToken}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("missing-key RecordFinal() error = %v, want rejection", err)
	}
	entry, err := outbox.Get(ctx, capture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Status != statusReserved || entry.Terminal != nil || entry.TerminalCredentialCiphertext != nil {
		t.Fatalf("rejected console checkpoint mutated state: %+v", entry)
	}
}

func TestLoadRejectsInvalidTerminalCredentialAssociationAndShape(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate func(*Entry)
	}{
		{name: "terminal nil", mutate: func(entry *Entry) { entry.Terminal = nil }},
		{name: "non-console terminal", mutate: func(entry *Entry) {
			entry.Terminal = &TerminalAction{Kind: TerminalSessionExit, SessionID: "session-1", ExitCode: 7}
		}},
		{name: "malformed nonce", mutate: func(entry *Entry) {
			entry.TerminalCredentialCiphertext.Nonce = entry.TerminalCredentialCiphertext.Nonce[:len(entry.TerminalCredentialCiphertext.Nonce)-1]
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "outbox")
			options := testOptions(dir, 4)
			options.SensitiveDataKey = []byte("terminal-validation-key")
			outbox, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			capture := testCapture("terminal-validation", false)
			if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: testSources(t, nil, false)}); err != nil {
				t.Fatal(err)
			}
			entry, err := outbox.RecordFinal(ctx, capture.ID, Final{
				Verdict: "failed", ExitCode: intPointer(7),
				Terminal: &TerminalAction{Kind: TerminalConsoleExit, SessionID: "session-1", SessionToken: "private-session-token", ExitCode: 7},
			})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&entry)
			encoded, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, capture.ID, stateFileName), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := outbox.Get(ctx, capture.ID); !errors.Is(err, ErrInvalidOutbox) {
				t.Fatalf("Get() error = %v, want invalid terminal credential state", err)
			}
		})
	}
}

func TestConsoleTerminalCredentialIsEncryptedResolvedAndAcknowledged(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "outbox")
	options := testOptions(dir, 4)
	options.SensitiveDataKey = []byte("terminal-credential-key")
	outbox, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	sources := testSources(t, nil, false)
	capture := testCapture("terminal-replay", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	terminal := &TerminalAction{Kind: TerminalConsoleExit, SessionID: "session-1", SessionToken: "private-session-token", ExitCode: 7}
	checkpoint, err := outbox.RecordFinal(ctx, capture.ID, Final{Verdict: "failed", ExitCode: intPointer(7), Terminal: terminal})
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Terminal == nil || checkpoint.Terminal.SessionToken != "" || checkpoint.TerminalCredentialCiphertext == nil {
		t.Fatalf("console checkpoint persisted an invalid terminal action: %+v", checkpoint)
	}
	statePath := filepath.Join(dir, capture.ID, stateFileName)
	state := mustRead(t, statePath)
	if bytes.Contains(state, []byte(terminal.SessionToken)) || bytes.Contains(state, []byte(`"session_token"`)) {
		t.Fatalf("state contains plaintext terminal credential: %s", state)
	}
	if !bytes.Contains(state, []byte(`"terminal_credential_ciphertext"`)) {
		t.Fatalf("state has no encrypted terminal credential: %s", state)
	}
	checkpoint.TerminalCredentialCiphertext.Nonce[0] ^= 0xff
	if _, err := outbox.RecordFinal(ctx, capture.ID, Final{Verdict: "failed", ExitCode: intPointer(7), Terminal: terminal}); err != nil {
		t.Fatalf("same terminal retry: %v", err)
	}
	different := *terminal
	different.SessionToken = "different-session-token"
	if _, err := outbox.RecordFinal(ctx, capture.ID, Final{Verdict: "failed", ExitCode: intPointer(7), Terminal: &different}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("different terminal retry error = %v, want conflict", err)
	}

	reloaded, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := reloaded.ResolveTerminal(ctx, capture.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, terminal) {
		t.Fatalf("resolved terminal = %+v, want %+v", resolved, terminal)
	}
	wrongOptions := options
	wrongOptions.SensitiveDataKey = []byte("wrong-terminal-credential-key")
	wrongKey, err := New(wrongOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongKey.ResolveTerminal(ctx, capture.ID); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("wrong-key ResolveTerminal() error = %v, want authenticated-decryption failure", err)
	}
	missingKey, err := New(testOptions(dir, 4))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missingKey.ResolveTerminal(ctx, capture.ID); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("missing-key ResolveTerminal() error = %v, want failure", err)
	}

	capture.State = "complete"
	capture.ExecutionVerdict = "failed"
	capture.Version++
	capture.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := reloaded.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	pending, err := reloaded.ListPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Status != statusComplete || pending[0].Terminal == nil || pending[0].Terminal.SessionToken != "" || pending[0].TerminalCredentialCiphertext == nil {
		t.Fatalf("terminal replay entry = %+v", pending)
	}
	if removed, err := reloaded.PruneCompleted(ctx, CompletedRetention{MaxEntries: 1}); err != nil || removed != 0 {
		t.Fatalf("PruneCompleted() = %d, %v; pending terminal action must be retained", removed, err)
	}
	if err := reloaded.AcknowledgeTerminal(ctx, capture.ID); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.AcknowledgeTerminal(ctx, capture.ID); err != nil {
		t.Fatalf("repeated acknowledgement: %v", err)
	}
	pending, err = reloaded.ListPending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after acknowledgement = %+v, %v", pending, err)
	}
	state = mustRead(t, statePath)
	if bytes.Contains(state, []byte(terminal.SessionToken)) || bytes.Contains(state, []byte(`"terminal_credential_ciphertext"`)) {
		t.Fatalf("acknowledged terminal action retained its credential: %s", state)
	}
}

func TestDiscardTerminalIsRepeatableBeforeAndAfterCaptureCompletion(t *testing.T) {
	ctx := context.Background()
	options := testOptions(filepath.Join(t.TempDir(), "outbox"), 4)
	options.SensitiveDataKey = []byte("discard-terminal-credential-key")
	outbox, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	sources := testSources(t, nil, false)

	for _, complete := range []bool{false, true} {
		capture := testCapture(fmt.Sprintf("discard-terminal-%t", complete), false)
		reservation := testReservation(capture)
		if _, err := outbox.RecordReservation(ctx, Reservation{Response: reservation, Sources: sources}); err != nil {
			t.Fatal(err)
		}
		terminal := &TerminalAction{Kind: TerminalConsoleExit, SessionID: "session-1", SessionToken: "private-session-token", ExitCode: 7}
		if _, err := outbox.RecordFinal(ctx, capture.ID, Final{Verdict: "failed", ExitCode: intPointer(7), Terminal: terminal}); err != nil {
			t.Fatal(err)
		}
		if complete {
			capture.State = "complete"
			capture.ExecutionVerdict = "failed"
			capture.Version++
			capture.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
			reservation.Capture = capture
			if _, err := outbox.RecordReservation(ctx, Reservation{Response: reservation, Sources: sources}); err != nil {
				t.Fatal(err)
			}
		}

		if err := outbox.DiscardTerminal(ctx, capture.ID); err != nil {
			t.Fatal(err)
		}
		if err := outbox.DiscardTerminal(ctx, capture.ID); err != nil {
			t.Fatalf("repeated discard: %v", err)
		}
		entry, err := outbox.Get(ctx, capture.ID)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Terminal != nil || entry.TerminalCredentialCiphertext != nil {
			t.Fatalf("discarded terminal action retained credential state: %+v", entry)
		}
		state := mustRead(t, filepath.Join(outbox.root, capture.ID, stateFileName))
		if bytes.Contains(state, []byte(terminal.SessionToken)) || bytes.Contains(state, []byte(`"terminal_credential_ciphertext"`)) {
			t.Fatalf("discarded terminal action retained its credential: %s", state)
		}
	}

	pending, err := outbox.ListPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Capture.ID != "discard-terminal-false" || pending[0].Terminal != nil {
		t.Fatalf("pending after terminal discard = %+v", pending)
	}
}

func TestPruneCompletedRejectsUnscrubbedTombstone(t *testing.T) {
	ctx := context.Background()
	sources := testSources(t, nil, false)
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("unscrubbed", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	capture.State = "complete"
	capture.Version++
	capture.CompletedAt = time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(outbox.root, capture.ID, stateFileName)
	entry := mustLoad(t, outbox, capture.ID)
	entry.Sources = sources
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.PruneCompleted(ctx, CompletedRetention{MaxEntries: 1}); !errors.Is(err, ErrInvalidOutbox) {
		t.Fatalf("PruneCompleted() error = %v, want invalid scrubbed tombstone", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("invalid tombstone was removed: %v", err)
	}
}

func TestPublishAbandonsUploadWhenAcknowledgementIsInvalid(t *testing.T) {
	ctx := context.Background()
	outbox := newTestOutbox(t, filepath.Join(t.TempDir(), "outbox"), 4)
	capture := testCapture("invalid-upload-ack", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: testSources(t, nil, false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "succeeded", ExitCode: intPointer(0)}); err != nil {
		t.Fatal(err)
	}
	baseClient := newCrashClient(capture)
	client := &invalidUploadAcknowledgementClient{crashClient: baseClient}
	var invalidErr error
	for attempt := 0; attempt < 20; attempt++ {
		err := outbox.Publish(ctx, client, capture.ID)
		if errors.Is(err, ErrInvalidOutbox) {
			invalidErr = err
			break
		}
		if err == nil || !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("pre-upload replay %d error = %v", attempt, err)
		}
	}
	if invalidErr == nil {
		t.Fatal("publish never reached invalid upload acknowledgement")
	}
	baseClient.mu.Lock()
	remainingUploads := len(baseClient.uploads)
	baseClient.mu.Unlock()
	if remainingUploads != 0 {
		t.Fatalf("temporary uploads after invalid acknowledgement = %d, want 0", remainingUploads)
	}
}

func TestPublishAbandonsUploadWhenAcknowledgementCheckpointFails(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "outbox")
	outbox := newTestOutbox(t, dir, 4)
	capture := testCapture("upload-checkpoint", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: testSources(t, nil, false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "succeeded", ExitCode: intPointer(0)}); err != nil {
		t.Fatal(err)
	}
	client := newCrashClient(capture)
	for attempt := 0; attempt < 20; attempt++ {
		err := outbox.Publish(ctx, client, capture.ID)
		client.mu.Lock()
		reachedUpload := false
		for key := range client.failed {
			if strings.HasPrefix(key, "upload:") {
				reachedUpload = true
				break
			}
		}
		client.mu.Unlock()
		if reachedUpload {
			if !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("first upload attempt error = %v", err)
			}
			break
		}
		if err == nil || !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("pre-upload replay %d error = %v", attempt, err)
		}
	}
	entryDir := filepath.Join(dir, capture.ID)
	if err := os.Chmod(entryDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(entryDir, 0700)
	if err := outbox.Publish(ctx, client, capture.ID); err == nil {
		t.Fatal("publish succeeded despite unwritable acknowledgement checkpoint")
	}
	client.mu.Lock()
	remainingUploads := len(client.uploads)
	client.mu.Unlock()
	if remainingUploads != 0 {
		t.Fatalf("temporary uploads after checkpoint failure = %d, want 0", remainingUploads)
	}
}

func TestReplaySurvivesReloadAndLostAcknowledgementsAtOperationBoundaries(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "outbox")
	outbox := newTestOutbox(t, dir, 4)
	sources := testSources(t, []byte("abcdefghij"), false)
	capture := testCapture("replay", false)
	if _, err := outbox.RecordReservation(ctx, Reservation{Response: testReservation(capture), Sources: sources}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Stage(ctx, capture.ID, Final{Verdict: "failed", ExitCode: intPointer(7), ErrorCode: "command_failed"}); err != nil {
		t.Fatal(err)
	}
	client := newCrashClient(capture)
	var completed bool
	for attempt := 0; attempt < 100; attempt++ {
		reloaded, err := New(testOptions(dir, 4))
		if err != nil {
			t.Fatal(err)
		}
		err = reloaded.ReplayAll(ctx, client)
		pending, listErr := reloaded.ListPending(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(pending) == 0 {
			if err != nil {
				t.Fatalf("completed replay returned %v", err)
			}
			completed = true
			break
		}
		if err == nil || !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("attempt %d error = %v, pending=%d", attempt, err, len(pending))
		}
	}
	if !completed {
		t.Fatal("replay did not complete")
	}
	if client.capture.State != "complete" || client.capture.ExecutionVerdict != "failed" || client.manifest.ID == "" {
		t.Fatalf("remote result: capture=%+v manifest=%+v", client.capture, client.manifest)
	}
	if len(client.artifacts) != 4 || len(client.segments) != 3 || client.workspaceSummary.ArtifactLogicalKey != "workspace/final" {
		t.Fatalf("published artifacts=%d segments=%d workspace=%+v", len(client.artifacts), len(client.segments), client.workspaceSummary)
	}
	// Completed entries are retained and replay becomes a no-op.
	before := client.callCount()
	reloaded, _ := New(testOptions(dir, 4))
	if err := reloaded.Publish(ctx, client, capture.ID); err != nil {
		t.Fatal(err)
	}
	if client.callCount() != before {
		t.Fatal("completed replay performed a network operation")
	}
	entryDir := filepath.Join(dir, capture.ID)
	if _, err := os.Stat(filepath.Join(entryDir, stateFileName)); err != nil {
		t.Fatalf("completion marker was not retained: %v", err)
	}
	objects, err := os.ReadDir(entryDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].Name() != stateFileName {
		t.Fatalf("completed outbox retained payload objects: %v", objects)
	}
	state := mustRead(t, filepath.Join(entryDir, stateFileName))
	for _, forbidden := range []string{"secret-upload-grant", workspaceFileName, "transcript-"} {
		if bytes.Contains(state, []byte(forbidden)) {
			t.Fatalf("completed tombstone retained %q: %s", forbidden, state)
		}
	}
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func testOptions(dir string, segmentBytes int64) Options {
	return Options{Dir: dir, SegmentBytes: segmentBytes, ArchiveLimits: historyarchive.DefaultLimits(), MaxOutstandingBytes: 64 << 20, MaxOutstandingEntries: 10}
}

func newTestOutbox(t *testing.T, dir string, segmentBytes int64) *Outbox {
	t.Helper()
	outbox, err := New(testOptions(dir, segmentBytes))
	if err != nil {
		t.Fatal(err)
	}
	return outbox
}

func testCapture(id string, harness bool) contract.HistoryCapture {
	capture := contract.HistoryCapture{ID: id, ProjectID: "project", JobID: "job-" + id, LeaseID: "lease-" + id, LeaseAttempt: 1, WorkerID: "worker", Role: "author", ExpectedTranscript: true, ExpectedHarness: harness, State: "reserved", ExecutionVerdict: "pending", Version: 1}
	if harness {
		capture.HarnessName, capture.HarnessVersion, capture.HarnessSchemaVersion = "harness", "v0.4.5", 5
	}
	return capture
}

func testReservation(capture contract.HistoryCapture) contract.ReserveHistoryCaptureResponse {
	return contract.ReserveHistoryCaptureResponse{Capture: capture, UploadGrant: "secret-upload-grant", Created: true}
}

func testSources(t *testing.T, transcript []byte, harness bool) SourcePaths {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "test@example.invalid")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(root, "transcript.txt")
	if err := os.WriteFile(transcriptPath, transcript, 0600); err != nil {
		t.Fatal(err)
	}
	sources := SourcePaths{Worktree: repo, Transcript: transcriptPath}
	if harness {
		native := filepath.Join(root, "native-root")
		if err := os.MkdirAll(filepath.Join(native, "children", "child"), 0700); err != nil {
			t.Fatal(err)
		}
		writeJSONFile(t, filepath.Join(native, "state.json"), map[string]any{"version": 5, "id": "native-root", "agent": "author", "model": "model", "build": map[string]any{"version": "v0.4.5"}})
		writeJSONFile(t, filepath.Join(native, "children", "child", "state.json"), map[string]any{"version": 5, "id": "native-child", "agent": "explore", "provider": "provider", "build": map[string]any{"version": "v0.4.5"}})
		writeJSONFile(t, filepath.Join(native, "children", "child", "meta.json"), map[string]any{"status": "completed"})
		sources.NativeSessionRoot, sources.NativeSessionID = native, "native-root"
	}
	return sources
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T, outbox *Outbox, id string) Entry {
	t.Helper()
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	entry, err := outbox.loadLocked(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertPrivateTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, dir os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := dir.Info()
		if err != nil {
			return err
		}
		if dir.IsDir() && info.Mode().Perm() != 0700 {
			t.Errorf("directory %s mode = %o", path, info.Mode().Perm())
		}
		if !dir.IsDir() && info.Mode().Perm() != 0600 {
			t.Errorf("file %s mode = %o", path, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func intPointer(value int) *int { return &value }

var errSimulatedCrash = errors.New("simulated process crash after operation boundary")

type invalidUploadAcknowledgementClient struct{ *crashClient }

func (c *invalidUploadAcknowledgementClient) UploadHistoryArtifactBytes(ctx context.Context, captureID, grant string, reader io.Reader) (contract.HistoryUploadResponse, error) {
	response, err := c.crashClient.UploadHistoryArtifactBytes(ctx, captureID, grant, reader)
	if err == nil {
		response.SHA256 = strings.Repeat("0", 64)
	}
	return response, err
}

type crashClient struct {
	mu               sync.Mutex
	capture          contract.HistoryCapture
	failed           map[string]bool
	calls            int
	uploads          map[string][]byte
	artifacts        map[string]contract.HistoryArtifact
	segments         map[string]contract.RegisterHistoryTranscriptSegmentRequest
	seal             *contract.HistoryTranscriptSeal
	expected         *contract.DeclareHistoryExpectedSetRequest
	workspaceSummary contract.RegisterHistoryWorkspaceSummaryRequest
	manifest         contract.HistoryArtifact
}

func newCrashClient(capture contract.HistoryCapture) *crashClient {
	return &crashClient{capture: capture, failed: map[string]bool{}, uploads: map[string][]byte{}, artifacts: map[string]contract.HistoryArtifact{}, segments: map[string]contract.RegisterHistoryTranscriptSegmentRequest{}}
}

func (c *crashClient) callCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }

func (c *crashClient) failBefore(key string) error {
	c.calls++
	if !c.failed[key] {
		c.failed[key] = true
		return errSimulatedCrash
	}
	return nil
}

func (c *crashClient) failAfter(key string) error {
	c.calls++
	if !c.failed[key] {
		c.failed[key] = true
		return errSimulatedCrash
	}
	return nil
}

func (c *crashClient) UploadHistoryArtifactBytes(_ context.Context, captureID, grant string, reader io.Reader) (contract.HistoryUploadResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := io.ReadAll(reader)
	if err != nil {
		return contract.HistoryUploadResponse{}, err
	}
	key := "upload:" + digest(data)
	if err := c.failBefore(key); err != nil {
		return contract.HistoryUploadResponse{}, err
	}
	id := "upload-" + digest(data)
	c.uploads[id] = append([]byte(nil), data...)
	return contract.HistoryUploadResponse{TemporaryUploadID: id, SHA256: digest(data), StoredSize: int64(len(data))}, nil
}

func (c *crashClient) AbandonHistoryArtifactUpload(_ context.Context, _, _, temporaryUploadID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.uploads[temporaryUploadID]; !ok {
		return errors.New("unknown upload")
	}
	delete(c.uploads, temporaryUploadID)
	c.calls++
	return nil
}

func (c *crashClient) PublishHistoryArtifact(_ context.Context, captureID, grant string, input contract.PublishHistoryArtifactRequest) (contract.HistoryArtifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.artifacts[input.LogicalKey]; ok {
		c.calls++
		return existing, nil
	}
	data, ok := c.uploads[input.TemporaryUploadID]
	if !ok {
		return contract.HistoryArtifact{}, errors.New("unknown upload")
	}
	artifact := contract.HistoryArtifact{ID: "artifact-" + input.LogicalKey, CaptureID: captureID, LogicalKey: input.LogicalKey, Kind: input.Kind, Phase: input.Phase, ArchiveID: input.ArchiveID, MediaType: input.MediaType, FormatVersion: input.FormatVersion, SchemaVersion: input.SchemaVersion, SHA256: digest(data), StoredSize: int64(len(data)), LogicalSize: input.LogicalSize, EntryCount: input.EntryCount, PublicationState: "committed"}
	c.artifacts[input.LogicalKey] = artifact
	if err := c.failAfter("publish:" + input.LogicalKey); err != nil {
		return contract.HistoryArtifact{}, err
	}
	return artifact, nil
}

func (c *crashClient) RegisterHistoryTranscriptSegment(_ context.Context, captureID, grant string, input contract.RegisterHistoryTranscriptSegmentRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := fmt.Sprintf("segment:%d:%d", input.Epoch, input.Sequence)
	if existing, ok := c.segments[key]; ok {
		c.calls++
		if !reflect.DeepEqual(existing, input) {
			return errors.New("segment differs")
		}
		return nil
	}
	c.segments[key] = input
	return c.failAfter(key)
}

func (c *crashClient) SealHistoryTranscript(_ context.Context, captureID, grant string, input contract.HistoryTranscriptSeal) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seal != nil {
		c.calls++
		if *c.seal != input {
			return errors.New("seal differs")
		}
		return nil
	}
	copy := input
	c.seal = &copy
	return c.failAfter("seal")
}

func (c *crashClient) DeclareHistoryExpectedSet(_ context.Context, captureID, grant string, input contract.DeclareHistoryExpectedSetRequest) (contract.HistoryCapture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expected != nil {
		c.calls++
		return c.capture, nil
	}
	copy := input
	c.expected = &copy
	c.capture.Version++
	if err := c.failAfter("expected"); err != nil {
		return contract.HistoryCapture{}, err
	}
	return c.capture, nil
}

func (c *crashClient) RegisterHistoryWorkspaceSummary(_ context.Context, captureID, grant string, input contract.RegisterHistoryWorkspaceSummaryRequest) (contract.HistoryWorkspaceSummary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.workspaceSummary.ArtifactLogicalKey != "" {
		c.calls++
		return contract.HistoryWorkspaceSummary{ArtifactID: c.artifacts[input.ArtifactLogicalKey].ID}, nil
	}
	c.workspaceSummary = input
	if err := c.failAfter("workspace"); err != nil {
		return contract.HistoryWorkspaceSummary{}, err
	}
	return contract.HistoryWorkspaceSummary{ArtifactID: c.artifacts[input.ArtifactLogicalKey].ID}, nil
}

func (c *crashClient) RegisterHistoryHarnessMembers(context.Context, string, string, contract.RegisterHistoryHarnessMembersRequest) error {
	return errors.New("unexpected Harness members")
}

func (c *crashClient) RecordHistoryExecutionVerdict(_ context.Context, captureID, grant string, input contract.RecordHistoryExecutionVerdictRequest) (contract.HistoryCapture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capture.ExecutionVerdict != "pending" {
		c.calls++
		return c.capture, nil
	}
	c.capture.ExecutionVerdict, c.capture.ExecutionExitCode, c.capture.ExecutionErrorCode = input.Verdict, input.ExitCode, input.ErrorCode
	c.capture.Version++
	if err := c.failAfter("verdict"); err != nil {
		return contract.HistoryCapture{}, err
	}
	return c.capture, nil
}

func (c *crashClient) TransitionHistoryCapture(_ context.Context, captureID, grant string, input contract.TransitionHistoryCaptureRequest) (contract.HistoryCapture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capture.State == input.To {
		c.calls++
		return c.capture, nil
	}
	legal := map[string]string{"reserved": "running", "running": "quiescing", "quiescing": "sealed", "sealed": "uploading"}
	if legal[c.capture.State] != input.To || input.ExpectedVersion != c.capture.Version {
		return contract.HistoryCapture{}, errors.New("illegal transition")
	}
	c.capture.State = input.To
	c.capture.Version++
	if err := c.failAfter("transition:" + input.To); err != nil {
		return contract.HistoryCapture{}, err
	}
	return c.capture, nil
}

func (c *crashClient) GenerateHistoryManifest(_ context.Context, captureID, grant string) (contract.HistoryArtifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.manifest.ID != "" {
		c.calls++
		return c.manifest, nil
	}
	c.manifest = contract.HistoryArtifact{ID: "manifest", CaptureID: captureID, LogicalKey: "manifest/final", Kind: "manifest", PublicationState: "committed"}
	if err := c.failAfter("manifest"); err != nil {
		return contract.HistoryArtifact{}, err
	}
	return c.manifest, nil
}

func (c *crashClient) CompleteHistoryCapture(_ context.Context, captureID, grant string, expectedVersion int64) (contract.HistoryCapture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capture.State == "complete" {
		c.calls++
		return c.capture, nil
	}
	if expectedVersion != c.capture.Version {
		return contract.HistoryCapture{}, errors.New("version differs")
	}
	c.capture.State = "complete"
	c.capture.Version++
	if err := c.failAfter("complete"); err != nil {
		return contract.HistoryCapture{}, err
	}
	return c.capture, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

var _ Client = (*crashClient)(nil)
var _ = strings.Builder{}
