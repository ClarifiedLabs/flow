package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestHistoryStorageMetricsExposeOnlyBoundedLabels(t *testing.T) {
	registry := New()
	history := RegisterHistoryStorage(registry, "s3")
	history.ObservePublication("success", 1024)
	history.ObservePublication("error", 0)
	history.ObserveSuccess(2, 1, 3, true, time.Unix(1234, 0))
	history.ObserveFailure()

	var output bytes.Buffer
	registry.Render(&output)
	text := output.String()
	for _, want := range []string{
		`flow_history_blob_store_info{backend="s3"} 1`,
		`flow_history_blob_publications_total{result="success"} 1`,
		`flow_history_blob_published_bytes_total 1024`,
		`flow_history_reconciliation_runs_total{result="success"} 1`,
		`flow_history_reconciliation_runs_total{result="error"} 1`,
		`flow_history_reconciliation_items_total{kind="temporary_removed"} 2`,
		`flow_history_reconciliation_items_total{kind="multipart_aborted"} 1`,
		`flow_history_reconciliation_orphans 3`,
		`flow_history_reconciliation_truncated 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"project=", "task=", "path=", "bucket=", "endpoint=", "key="} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("metrics contain forbidden label %q:\n%s", forbidden, text)
		}
	}
}
