package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DQC-02 observation (ZS-SVC-Z-001 sections 6 and 11, POST /dqc/observations).
//
// An observation is the recorded fact that an asset's data was seen over a window at a source watermark,
// with the hash of the evidence that was seen. As everywhere in this service, the CALLER (the owning
// domain) observes the data and asserts it; this service records the assertion, immutably, and never
// reads the data itself. Late-arriving evidence is a NEW observation at a later watermark: an observation
// is never changed (section 12).

const PrefixObservation = "dqo_"

var evidenceHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

type RecordObservationRequest struct {
	AssetID         string    `json:"asset_id"`
	WindowStart     time.Time `json:"window_start"`
	WindowEnd       time.Time `json:"window_end"`
	SourceWatermark string    `json:"source_watermark"`
	SourceSystem    string    `json:"source_system"`
	RowCount        int64     `json:"row_count"`
	EvidenceHash    string    `json:"evidence_hash"`
}

func (r RecordObservationRequest) Validate() error {
	if strings.TrimSpace(r.AssetID) == "" {
		return fmt.Errorf("asset_id is required")
	}
	if r.WindowStart.IsZero() || r.WindowEnd.IsZero() {
		return fmt.Errorf("window_start and window_end are required")
	}
	if !r.WindowEnd.After(r.WindowStart) {
		return fmt.Errorf("window_end must be after window_start")
	}
	if strings.TrimSpace(r.SourceWatermark) == "" || len(r.SourceWatermark) > 256 {
		return fmt.Errorf("source_watermark is required (at most 256 characters): an observation without one cannot say what it saw")
	}
	if strings.TrimSpace(r.SourceSystem) == "" || len(r.SourceSystem) > 128 {
		return fmt.Errorf("source_system is required (at most 128 characters)")
	}
	if r.RowCount < 0 {
		return fmt.Errorf("row_count cannot be negative")
	}
	if !evidenceHashRe.MatchString(r.EvidenceHash) {
		return fmt.Errorf("evidence_hash must be 64 lowercase hex characters")
	}
	if r.WindowEnd.After(time.Now().Add(MaxEvidenceClockSkew)) {
		return fmt.Errorf("window_end is in the future")
	}
	return nil
}

type DQObservation struct {
	ObservationRunID string    `json:"observation_run_id"`
	TenantID         string    `json:"tenant_id"`
	AssetID          string    `json:"asset_id"`
	WindowStart      time.Time `json:"window_start"`
	WindowEnd        time.Time `json:"window_end"`
	SourceWatermark  string    `json:"source_watermark"`
	SourceSystem     string    `json:"source_system"`
	RowCount         int64     `json:"row_count"`
	EvidenceHash     string    `json:"evidence_hash"`
	RecordedBy       string    `json:"recorded_by"`
	RecordedAt       time.Time `json:"recorded_at"`
	// Deduplicated is set on the response when the same observation was already recorded.
	Deduplicated bool `json:"deduplicated,omitempty"`
}
