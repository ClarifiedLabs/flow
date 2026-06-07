package coordinator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

func TestIdempotencyReserveCompleteAndConflict(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	service := NewIdempotencyService(store.DB())
	record := IdempotencyRecord{
		PrincipalKey:   "owner:owner:hash",
		IdempotencyKey: "create-1",
		Method:         "POST",
		Path:           "/v1/issues",
		RequestHash:    "hash-a",
	}

	reserved, ok, err := service.Reserve(ctx, record)
	if err != nil {
		t.Fatalf("reserve idempotency record: %v", err)
	}
	if !ok {
		t.Fatal("first reservation did not reserve")
	}
	if reserved.StatusCode != IdempotencyPendingStatus {
		t.Fatalf("reserved status = %d, want pending", reserved.StatusCode)
	}

	again, ok, err := service.Reserve(ctx, record)
	if err != nil {
		t.Fatalf("reserve same idempotency record: %v", err)
	}
	if ok {
		t.Fatal("second reservation unexpectedly reserved")
	}
	if again.StatusCode != IdempotencyPendingStatus {
		t.Fatalf("second status = %d, want pending", again.StatusCode)
	}

	record.StatusCode = 201
	record.ResponseBody = []byte(`{"issue":{"id":"i-0001"}}`)
	if err := service.Complete(ctx, record); err != nil {
		t.Fatalf("complete idempotency record: %v", err)
	}

	completed, ok, err := service.Reserve(ctx, record)
	if err != nil {
		t.Fatalf("reserve completed idempotency record: %v", err)
	}
	if ok {
		t.Fatal("completed reservation unexpectedly reserved")
	}
	if completed.StatusCode != 201 || string(completed.ResponseBody) != string(record.ResponseBody) {
		t.Fatalf("completed record = %+v", completed)
	}

	conflict := record
	conflict.RequestHash = "hash-b"
	conflicting, ok, err := service.Reserve(ctx, conflict)
	if err != nil {
		t.Fatalf("reserve conflicting idempotency record: %v", err)
	}
	if ok {
		t.Fatal("conflicting reservation unexpectedly reserved")
	}
	if conflicting.RequestHash != record.RequestHash {
		t.Fatalf("conflicting record hash = %q, want %q", conflicting.RequestHash, record.RequestHash)
	}
}

func TestIdempotencyCancelAllowsRetryAfterFailedMutation(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	service := NewIdempotencyService(store.DB())
	record := IdempotencyRecord{
		PrincipalKey:   "owner:owner:hash",
		IdempotencyKey: "failed-create",
		Method:         "POST",
		Path:           "/v1/issues",
		RequestHash:    "hash-a",
	}
	if _, ok, err := service.Reserve(ctx, record); err != nil || !ok {
		t.Fatalf("reserve failed mutation record ok=%v err=%v", ok, err)
	}
	if err := service.Cancel(ctx, record.PrincipalKey, record.IdempotencyKey, record.Method, record.Path, record.RequestHash); err != nil {
		t.Fatalf("cancel idempotency record: %v", err)
	}
	if _, ok, err := service.Reserve(ctx, record); err != nil || !ok {
		t.Fatalf("reserve after cancel ok=%v err=%v", ok, err)
	}
}

func TestIdempotencyReserveReclaimsStalePendingAfterTTL(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	service := NewIdempotencyService(store.DB())
	service.now = func() time.Time { return now }

	record := IdempotencyRecord{
		PrincipalKey:   "owner:owner:hash",
		IdempotencyKey: "stale-pending",
		Method:         "POST",
		Path:           "/v1/issues",
		RequestHash:    "hash-a",
	}

	reserved, ok, err := service.Reserve(ctx, record)
	if err != nil || !ok {
		t.Fatalf("initial reserve ok=%v err=%v", ok, err)
	}
	if reserved.StatusCode != IdempotencyPendingStatus {
		t.Fatalf("reserved status = %d, want pending", reserved.StatusCode)
	}

	// Advance the injected clock past the TTL; a retry should reclaim the wedge.
	now = now.Add(IdempotencyPendingTTL + time.Second)

	again, ok, err := service.Reserve(ctx, record)
	if err != nil {
		t.Fatalf("reserve after TTL: %v", err)
	}
	if !ok {
		t.Fatal("stale pending reservation was not reclaimed")
	}
	if again.StatusCode != IdempotencyPendingStatus {
		t.Fatalf("reclaimed status = %d, want pending", again.StatusCode)
	}
	if !again.CreatedAt.Equal(now) {
		t.Fatalf("reclaimed created_at = %v, want fresh %v", again.CreatedAt, now)
	}
}

func TestIdempotencyReserveDoesNotReclaimPendingBeforeTTL(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	start := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	now := start
	service := NewIdempotencyService(store.DB())
	service.now = func() time.Time { return now }

	record := IdempotencyRecord{
		PrincipalKey:   "owner:owner:hash",
		IdempotencyKey: "fresh-pending",
		Method:         "POST",
		Path:           "/v1/issues",
		RequestHash:    "hash-a",
	}

	if _, ok, err := service.Reserve(ctx, record); err != nil || !ok {
		t.Fatalf("initial reserve ok=%v err=%v", ok, err)
	}

	// Advance to just under the TTL; the original 102 row must remain intact.
	now = now.Add(IdempotencyPendingTTL - time.Second)

	again, ok, err := service.Reserve(ctx, record)
	if err != nil {
		t.Fatalf("reserve before TTL: %v", err)
	}
	if ok {
		t.Fatal("pending reservation was reclaimed prematurely")
	}
	if again.StatusCode != IdempotencyPendingStatus {
		t.Fatalf("status = %d, want pending", again.StatusCode)
	}
	if !again.CreatedAt.Equal(start) {
		t.Fatalf("created_at = %v, want original %v", again.CreatedAt, start)
	}
}

func TestIdempotencyReserveReplaysCompletedRecordDespiteAge(t *testing.T) {
	ctx := context.Background()
	store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	service := NewIdempotencyService(store.DB())
	service.now = func() time.Time { return now }

	record := IdempotencyRecord{
		PrincipalKey:   "owner:owner:hash",
		IdempotencyKey: "completed",
		Method:         "POST",
		Path:           "/v1/issues",
		RequestHash:    "hash-a",
	}

	if _, ok, err := service.Reserve(ctx, record); err != nil || !ok {
		t.Fatalf("initial reserve ok=%v err=%v", ok, err)
	}

	record.StatusCode = 201
	record.ResponseBody = []byte(`{"issue":{"id":"i-0001"}}`)
	if err := service.Complete(ctx, record); err != nil {
		t.Fatalf("complete idempotency record: %v", err)
	}

	// Advance the clock far past the TTL; completed rows must never be reclaimed.
	now = now.Add(10 * IdempotencyPendingTTL)

	completed, ok, err := service.Reserve(ctx, record)
	if err != nil {
		t.Fatalf("reserve completed record: %v", err)
	}
	if ok {
		t.Fatal("completed reservation was unexpectedly reclaimed")
	}
	if completed.StatusCode != 201 || string(completed.ResponseBody) != string(record.ResponseBody) {
		t.Fatalf("completed record = %+v", completed)
	}
}
