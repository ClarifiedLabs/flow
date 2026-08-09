package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/blob"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

type publicationGateStore struct {
	blob.Store
	fail bool
}

func (s *publicationGateStore) Publish(ctx context.Context, temporary blob.Temporary, key blob.Key) (blob.Object, error) {
	if s.fail {
		return blob.Object{}, errors.New("publication unavailable")
	}
	return s.Store.Publish(ctx, temporary, key)
}

func newHistoryRegistry(t *testing.T, supplied blob.Store) (*Registry, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	global, err := flowdb.OpenGlobal(ctx, filepath.Join(dataDir, "global.db"))
	if err != nil {
		t.Fatalf("open global database: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })
	registry, err := NewRegistry(RegistryOptions{DataDir: dataDir, Global: global, HistoryBlobStore: supplied})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry, dataDir
}

func TestRegistryHistoryStoreDefaultsAndExternalOwnership(t *testing.T) {
	t.Parallel()
	defaultRegistry, dataDir := newHistoryRegistry(t, nil)
	if defaultRegistry.HistoryBlobStore() == nil || !defaultRegistry.OwnsHistoryBlobStore() {
		t.Fatalf("default history store = %T owned=%t", defaultRegistry.HistoryBlobStore(), defaultRegistry.OwnsHistoryBlobStore())
	}
	upload, err := defaultRegistry.HistoryBlobStore().Begin(context.Background())
	if err != nil {
		t.Fatalf("default history store beneath %s is unusable: %v", dataDir, err)
	}
	if err := upload.Abort(context.Background()); err != nil {
		t.Fatalf("abort default store probe: %v", err)
	}
	if err := defaultRegistry.Close(); err != nil {
		t.Fatalf("close default registry: %v", err)
	}
	if _, err := defaultRegistry.HistoryBlobStore().Begin(context.Background()); !errors.Is(err, blob.ErrStoreClosed) {
		t.Fatalf("owned history store remains open after registry close: %v", err)
	}

	external, err := blob.NewLocal(filepath.Join(t.TempDir(), "external"), blob.LocalOptions{})
	if err != nil {
		t.Fatalf("new external store: %v", err)
	}
	externalRegistry, _ := newHistoryRegistry(t, external)
	if externalRegistry.HistoryBlobStore() != external || externalRegistry.OwnsHistoryBlobStore() {
		t.Fatalf("external store identity/ownership = %T/%t", externalRegistry.HistoryBlobStore(), externalRegistry.OwnsHistoryBlobStore())
	}
	if err := externalRegistry.Close(); err != nil {
		t.Fatalf("close external-store registry: %v", err)
	}
	externalUpload, err := external.Begin(context.Background())
	if err != nil {
		t.Fatalf("registry closed caller-owned history store: %v", err)
	}
	if err := externalUpload.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := external.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryWiresProjectIsolatedHistoryServicesAndMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local, err := blob.NewLocal(filepath.Join(t.TempDir(), "blobs"), blob.LocalOptions{})
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}
	gate := &publicationGateStore{Store: local}
	registry, _ := newHistoryRegistry(t, gate)
	firstProject, err := registry.CreateProject(ctx, coordinator.Project{Name: "history-one", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create first project: %v", err)
	}
	secondProject, err := registry.CreateProject(ctx, coordinator.Project{Name: "history-two", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}
	first, _ := registry.Bundle(firstProject.ID)
	second, _ := registry.Bundle(secondProject.ID)
	if first.HistoryCaptures == nil || second.HistoryCaptures == nil || first.HistoryCaptures == second.HistoryCaptures {
		t.Fatalf("history services are not project-isolated: %p %p", first.HistoryCaptures, second.HistoryCaptures)
	}

	pendingTemporary := reserveAndUploadHistoryArtifact(t, gate, first.HistoryCaptures, firstProject.ID, "job-one", true)
	reserveAndUploadHistoryArtifact(t, gate, first.HistoryCaptures, firstProject.ID, "job-one-more", true)
	reserveAndUploadHistoryArtifact(t, gate, second.HistoryCaptures, secondProject.ID, "job-two", true)
	reserveAndUploadHistoryArtifact(t, gate, second.HistoryCaptures, secondProject.ID, "job-two-more", true)
	activeTemporary := reserveAndCompleteHistoryUpload(t, second.HistoryCaptures, secondProject.ID, "job-active")

	_, complete, err := registry.HistoryTemporaryProtection(ctx, 1)
	if err != nil {
		t.Fatalf("bounded history temporary protection: %v", err)
	}
	if complete {
		t.Fatal("history temporary protection with a one-row project allowance should be truncated")
	}
	protected, complete, err := registry.HistoryTemporaryProtection(ctx, 100)
	if err != nil {
		t.Fatalf("history temporary protection: %v", err)
	}
	if !complete {
		t.Fatal("history temporary protection unexpectedly truncated")
	}
	if _, ok := protected[pendingTemporary.ID]; !ok {
		t.Fatalf("pending temporary %s is not protected: %v", pendingTemporary.ID, protected)
	}
	if _, ok := protected[activeTemporary.ID]; !ok {
		t.Fatalf("active-intent temporary %s is not protected: %v", activeTemporary.ID, protected)
	}
	if len(protected) != 5 {
		t.Fatalf("protected temporaries = %d, want 5", len(protected))
	}

	gate.fail = false
	summary, err := registry.ReconcilePendingHistoryArtifacts(ctx, 1)
	if err != nil {
		t.Fatalf("reconcile pending history artifacts: %v", err)
	}
	if summary.Projects != 2 || summary.Examined != 2 || summary.Committed != 2 || summary.Pending != 0 || summary.Failed != 0 {
		t.Fatalf("bounded per-project reconciliation = %+v", summary)
	}
	protected, complete, err = registry.HistoryTemporaryProtection(ctx, 100)
	if err != nil {
		t.Fatalf("history temporary protection after reconciliation: %v", err)
	}
	if !complete {
		t.Fatal("history temporary protection after reconciliation unexpectedly truncated")
	}
	if _, ok := protected[activeTemporary.ID]; !ok || len(protected) != 3 {
		t.Fatalf("protected temporaries after reconciliation = %v, want three including %s", protected, activeTemporary.ID)
	}
	var firstCaptures, secondCaptures int
	if err := first.Store.DB().QueryRow(`SELECT count(*) FROM history_captures`).Scan(&firstCaptures); err != nil {
		t.Fatal(err)
	}
	if err := second.Store.DB().QueryRow(`SELECT count(*) FROM history_captures`).Scan(&secondCaptures); err != nil {
		t.Fatal(err)
	}
	if firstCaptures != 2 || secondCaptures != 3 {
		t.Fatalf("capture counts = %d/%d, want isolated 2/3", firstCaptures, secondCaptures)
	}
}

func reserveAndCompleteHistoryUpload(t *testing.T, service *coordinator.HistoryCaptureService, projectID, jobID string) blob.Temporary {
	t.Helper()
	ctx := context.Background()
	reserved, err := service.Reserve(ctx, coordinator.ReserveHistoryCaptureInput{
		ProjectID: projectID, JobID: jobID, LeaseID: "lease-" + jobID, LeaseAttempt: 1,
		WorkerID: "worker", Role: "author", ExpectedHarness: true,
		HarnessName: "harness", HarnessSchemaVersion: 5,
	})
	if err != nil {
		t.Fatalf("reserve history capture: %v", err)
	}
	upload, err := service.BeginUpload(ctx, reserved.Capture.ID, reserved.UploadGrant)
	if err != nil {
		t.Fatalf("begin history upload: %v", err)
	}
	if _, err := upload.Write([]byte(jobID)); err != nil {
		t.Fatalf("write history upload: %v", err)
	}
	temporary, err := upload.Complete(ctx)
	if err != nil {
		t.Fatalf("complete history upload: %v", err)
	}
	return temporary
}

func reserveAndUploadHistoryArtifact(t *testing.T, gate *publicationGateStore, service *coordinator.HistoryCaptureService, projectID, jobID string, fail bool) blob.Temporary {
	t.Helper()
	ctx := context.Background()
	reserved, err := service.Reserve(ctx, coordinator.ReserveHistoryCaptureInput{
		ProjectID: projectID, JobID: jobID, LeaseID: "lease-" + jobID, LeaseAttempt: 1,
		WorkerID: "worker", Role: "author", ExpectedHarness: true,
		HarnessName: "harness", HarnessSchemaVersion: 5,
	})
	if err != nil {
		t.Fatalf("reserve history capture: %v", err)
	}
	// Keep the reservation grant so this helper can exercise publication after
	// reserveAndCompleteHistoryUpload's shared upload path.
	upload, err := service.BeginUpload(ctx, reserved.Capture.ID, reserved.UploadGrant)
	if err != nil {
		t.Fatalf("begin history upload: %v", err)
	}
	if _, err := upload.Write([]byte(jobID)); err != nil {
		t.Fatalf("write history upload: %v", err)
	}
	temporary, err := upload.Complete(ctx)
	if err != nil {
		t.Fatalf("complete history upload: %v", err)
	}
	gate.fail = fail
	_, err = service.PublishArtifact(ctx, reserved.Capture.ID, reserved.UploadGrant, coordinator.PublishHistoryArtifactInput{
		LogicalKey: "harness/final/root", Kind: coordinator.HistoryArtifactHarnessRoot,
		Phase: coordinator.HistoryArtifactFinal, MediaType: "application/gzip", LogicalSize: int64(len(jobID)),
	}, temporary)
	if fail && err == nil {
		t.Fatal("publication unexpectedly succeeded")
	}
	if !fail && err != nil {
		t.Fatalf("publish history artifact: %v", err)
	}
	return temporary
}
