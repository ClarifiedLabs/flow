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
	defaultRegistry, dataDir := newHistoryRegistry(t, nil)
	if defaultRegistry.HistoryBlobStore() == nil || !defaultRegistry.OwnsHistoryBlobStore() {
		t.Fatalf("default history store = %T owned=%t", defaultRegistry.HistoryBlobStore(), defaultRegistry.OwnsHistoryBlobStore())
	}
	if _, err := defaultRegistry.HistoryBlobStore().Begin(context.Background()); err != nil {
		t.Fatalf("default history store beneath %s is unusable: %v", dataDir, err)
	}

	external, err := blob.NewLocal(filepath.Join(t.TempDir(), "external"), blob.LocalOptions{})
	if err != nil {
		t.Fatalf("new external store: %v", err)
	}
	externalRegistry, _ := newHistoryRegistry(t, external)
	if externalRegistry.HistoryBlobStore() != external || externalRegistry.OwnsHistoryBlobStore() {
		t.Fatalf("external store identity/ownership = %T/%t", externalRegistry.HistoryBlobStore(), externalRegistry.OwnsHistoryBlobStore())
	}
}

func TestRegistryWiresProjectIsolatedHistoryServicesAndMetadata(t *testing.T) {
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
	reserveAndUploadHistoryArtifact(t, gate, second.HistoryCaptures, secondProject.ID, "job-two", false)

	metadata, err := registry.HistoryBlobMetadata(ctx)
	if err != nil {
		t.Fatalf("history blob metadata: %v", err)
	}
	if _, ok := metadata.LiveTemporaryIDs[pendingTemporary.ID]; !ok || len(metadata.PendingKeys) != 1 || len(metadata.ReferencedKeys) != 1 {
		t.Fatalf("metadata = live:%v pending:%d referenced:%d", metadata.LiveTemporaryIDs, len(metadata.PendingKeys), len(metadata.ReferencedKeys))
	}
	var firstCaptures, secondCaptures int
	if err := first.Store.DB().QueryRow(`SELECT count(*) FROM history_captures`).Scan(&firstCaptures); err != nil {
		t.Fatal(err)
	}
	if err := second.Store.DB().QueryRow(`SELECT count(*) FROM history_captures`).Scan(&secondCaptures); err != nil {
		t.Fatal(err)
	}
	if firstCaptures != 1 || secondCaptures != 1 {
		t.Fatalf("capture counts = %d/%d, want isolated 1/1", firstCaptures, secondCaptures)
	}
}

func reserveAndUploadHistoryArtifact(t *testing.T, gate *publicationGateStore, service *coordinator.HistoryCaptureService, projectID, jobID string, fail bool) blob.Temporary {
	t.Helper()
	ctx := context.Background()
	reserved, err := service.Reserve(ctx, coordinator.ReserveHistoryCaptureInput{
		ProjectID: projectID, JobID: jobID, LeaseID: "lease-" + jobID, LeaseAttempt: 1,
		WorkerID: "worker", Role: "author", ExpectedHarness: true,
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
