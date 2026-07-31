package metrics

import "time"

// HistoryStorage is a deliberately low-cardinality metric set. Labels describe
// only backend/result/kind; project, task, capture, object key, path, endpoint,
// bucket, and credential data must never be attached.
type HistoryStorage struct {
	StoreInfo            *Gauge
	Publications         *Counter
	PublicationBytes     *Counter
	Reconciliations      *Counter
	ReconciledItems      *Counter
	ReportedOrphans      *Gauge
	LastSuccessTimestamp *Gauge
	Truncated            *Gauge
}

func RegisterHistoryStorage(registry *Registry, backend string) HistoryStorage {
	metrics := HistoryStorage{
		StoreInfo:            registry.Gauge("flow_history_blob_store_info", "Configured history blob backend (always 1)."),
		Publications:         registry.Counter("flow_history_blob_publications_total", "History blob publication outcomes."),
		PublicationBytes:     registry.Counter("flow_history_blob_published_bytes_total", "Bytes committed to immutable history blob storage."),
		Reconciliations:      registry.Counter("flow_history_reconciliation_runs_total", "History blob reconciliation passes by result."),
		ReconciledItems:      registry.Counter("flow_history_reconciliation_items_total", "History blob items handled by conservative reconciliation."),
		ReportedOrphans:      registry.Gauge("flow_history_reconciliation_orphans", "Published unreferenced blobs reported by the latest pass; they are not automatically deleted."),
		LastSuccessTimestamp: registry.Gauge("flow_history_reconciliation_last_success_timestamp_seconds", "Unix timestamp of the last successful history blob reconciliation."),
		Truncated:            registry.Gauge("flow_history_reconciliation_truncated", "Whether the latest bounded history reconciliation pass was truncated."),
	}
	metrics.StoreInfo.Set(1, map[string]string{"backend": backend})
	return metrics
}

func (m HistoryStorage) ObservePublication(result string, bytes int64) {
	m.Publications.Inc(map[string]string{"result": result})
	if result == "success" && bytes > 0 {
		m.PublicationBytes.Add(float64(bytes), nil)
	}
}

func (m HistoryStorage) ObserveSuccess(removedTemporary, abortedMultipart, orphans int, truncated bool, at time.Time) {
	m.Reconciliations.Inc(map[string]string{"result": "success"})
	if removedTemporary > 0 {
		m.ReconciledItems.Add(float64(removedTemporary), map[string]string{"kind": "temporary_removed"})
	}
	if abortedMultipart > 0 {
		m.ReconciledItems.Add(float64(abortedMultipart), map[string]string{"kind": "multipart_aborted"})
	}
	m.ReportedOrphans.Set(float64(orphans), nil)
	m.LastSuccessTimestamp.Set(float64(at.Unix()), nil)
	if truncated {
		m.Truncated.Set(1, nil)
	} else {
		m.Truncated.Set(0, nil)
	}
}

func (m HistoryStorage) ObserveFailure() {
	m.Reconciliations.Inc(map[string]string{"result": "error"})
}
