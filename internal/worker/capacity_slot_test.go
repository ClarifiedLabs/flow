package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
)

func TestCapacitySlotCapabilityReadinessIgnoresReasoningMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	slots := NewCapacitySlots(store.DB())
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	slots.now = func() time.Time { return now }
	expected := flowharness.Model{
		Harness: "harness", ProviderID: "openai", ModelID: "gpt-5",
		QualifiedID: "openai:gpt-5", Reasoning: flowharness.ReasoningInfo{Supported: true},
	}
	slot, err := slots.Create(ctx, CreateCapacitySlotInput{
		ProviderID: "kind", ProfileName: "harness", ProviderRequestID: "probe-1",
		ProviderType: "kubernetes", ProfileLabels: map[string]string{"agent.harness.harness": "true"},
		ProfileHarnessModels: []flowharness.Model{expected}, StartupDeadline: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	actual := Worker{
		ID: slot.WorkerID, Labels: map[string]string{"agent.harness.harness": "true"},
		HarnessModels: []flowharness.Model{{Harness: "harness", ProviderID: "openai", ModelID: "gpt-5", QualifiedID: "openai:gpt-5"}},
	}
	if err := WorkerSatisfiesSlot(slot, actual); err != nil {
		t.Fatalf("same qualified model with different reasoning metadata was rejected: %v", err)
	}
	ready, err := slots.RecordCapabilities(ctx, slot.ID, WorkerSatisfiesSlot(slot, actual))
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != CapacitySlotReady || ready.CapabilityCheckedAt == nil {
		t.Fatalf("ready slot = %+v", ready)
	}

	actual.HarnessModels = nil
	unready, err := slots.RecordCapabilities(ctx, slot.ID, WorkerSatisfiesSlot(slot, actual))
	if err != nil {
		t.Fatal(err)
	}
	if unready.State != CapacitySlotUnready || unready.CapabilityError == "" {
		t.Fatalf("unready slot = %+v", unready)
	}
}

func TestCapacitySlotSuccessfulProbeRetryRequiresFreshCapabilities(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	slots := NewCapacitySlots(store.DB())
	createdAt := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	slots.now = func() time.Time { return createdAt }
	slot, err := slots.Create(ctx, CreateCapacitySlotInput{
		ProviderID: "kind", ProfileName: "harness", ProviderRequestID: "probe-retry",
		ProviderType: "kubernetes", StartupDeadline: createdAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	unready, err := slots.RecordCapabilities(ctx, slot.ID, context.DeadlineExceeded)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := createdAt.Add(10 * time.Second)
	slots.now = func() time.Time { return retryAt }
	retrying, err := slots.RecordAttempt(ctx, unready.ID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if retrying.State != CapacitySlotProvisioning || retrying.CapabilityCheckedAt != nil || retrying.CapabilityError != "" {
		t.Fatalf("retrying slot = %+v", retrying)
	}
	if !retrying.StartupDeadline.Equal(retryAt.Add(time.Minute)) {
		t.Fatalf("startup deadline = %s, want %s", retrying.StartupDeadline, retryAt.Add(time.Minute))
	}
}

func TestEnqueueJobAddsTypedHarnessSelectorWithoutReasoning(t *testing.T) {
	ctx := context.Background()
	_, _, service := newWorkerService(t)
	job, err := service.EnqueueJob(ctx, EnqueueJobInput{
		Role: RoleCI, CapacityBucket: BucketEphemeral, Payload: map[string]any{"blocking": true},
		Harness: HarnessRequirement{Harness: "harness", Model: "openai:gpt-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Selector[flowharness.AgentHarnessLabel("harness")] != "true" {
		t.Fatalf("selector = %#v", job.Selector)
	}
	if job.Harness.Harness != "harness" || job.Harness.Model != "openai:gpt-5" {
		t.Fatalf("requirement = %+v", job.Harness)
	}
}
