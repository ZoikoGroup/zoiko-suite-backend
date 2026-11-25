package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/outbox"
)

const observationCols = `observation_run_id, tenant_id, asset_id, window_start, window_end, source_watermark, source_system, row_count, evidence_hash, recorded_by, recorded_at`

func scanObservation(row pgx.Row) (*domain.DQObservation, error) {
	var o domain.DQObservation
	if err := row.Scan(&o.ObservationRunID, &o.TenantID, &o.AssetID, &o.WindowStart, &o.WindowEnd, &o.SourceWatermark, &o.SourceSystem,
		&o.RowCount, &o.EvidenceHash, &o.RecordedBy, &o.RecordedAt); err != nil {
		return nil, err
	}
	return &o, nil
}

// RecordObservation records that an asset was observed over a window at a source watermark
// (POST /dqc/observations). The key is asset + window + watermark: the same key with the same evidence
// returns the existing observation, and the same key with DIFFERENT evidence is a conflict, because
// late-arriving evidence is a new observation at a later watermark, never an edit of an earlier one.
// The asset must be a registered DQC-01 asset.
func (s *PgStore) RecordObservation(ctx context.Context, tenantID string, req domain.RecordObservationRequest, actor string, claim domain.IdempotencyClaim) (*domain.DQObservation, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DQObservation
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dqc_assets WHERE asset_id = $1`, req.AssetID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return domain.ErrContractNotFound
		}
		existing, err := scanObservation(tx.QueryRow(ctx, `SELECT `+observationCols+` FROM dq_observations
			WHERE tenant_id = $1 AND asset_id = $2 AND window_start = $3 AND window_end = $4 AND source_watermark = $5`,
			tenantID, req.AssetID, req.WindowStart, req.WindowEnd, req.SourceWatermark))
		switch {
		case err == nil:
			if existing.EvidenceHash != req.EvidenceHash || existing.RowCount != req.RowCount || existing.SourceSystem != req.SourceSystem {
				return domain.ErrObservationConflict
			}
			existing.Deduplicated = true
			out = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		id := newID(domain.PrefixObservation)
		claim.ResourceID = id
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanObservation(tx.QueryRow(ctx, `
			INSERT INTO dq_observations (observation_run_id, tenant_id, asset_id, window_start, window_end, source_watermark, source_system,
				row_count, evidence_hash, recorded_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+observationCols,
			id, tenantID, req.AssetID, req.WindowStart, req.WindowEnd, req.SourceWatermark, req.SourceSystem, req.RowCount, req.EvidenceHash, actor))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_observation", AggregateID: got.ObservationRunID,
			EventType: "dqc.observation.completed", TenantID: &tenantID, Payload: map[string]any{
				"tenant_id": tenantID, "asset_id": got.AssetID, "observation_run_id": got.ObservationRunID,
				"window":    map[string]any{"start": got.WindowStart, "end": got.WindowEnd},
				"watermark": got.SourceWatermark, "evidence_hash": got.EvidenceHash, "row_count": got.RowCount, "source_system": got.SourceSystem,
			}})
	})
	return out, err
}

func (s *PgStore) GetObservation(ctx context.Context, tenantID, observationRunID string) (*domain.DQObservation, error) {
	var out *domain.DQObservation
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		o, err := scanObservation(tx.QueryRow(ctx, `SELECT `+observationCols+` FROM dq_observations WHERE observation_run_id = $1`, observationRunID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrObservationNotFound
		}
		out = o
		return err
	})
	return out, err
}

// evidenceFingerprint is the hash of exactly what a contract run is judged on: the contract version, the
// frozen population and when it was observed, and every rule outcome. Two requests with the same
// fingerprint are the same evidence. Outcomes are sorted by rule so their order does not matter.
func evidenceFingerprint(contractID string, req domain.RunDQRequest) string {
	type outcome struct {
		RuleKey string `json:"rule_key"`
		Pass    int64  `json:"pass"`
		Fail    int64  `json:"fail"`
		Sum     string `json:"sum"`
	}
	outs := make([]outcome, 0, len(req.RuleOutcomes))
	for _, o := range req.RuleOutcomes {
		outs = append(outs, outcome{o.RuleKey, o.PassCount, o.FailCount, o.FailureMagnitudeSum})
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].RuleKey < outs[j].RuleKey })
	asOf := ""
	if req.EvidenceAsOf != nil {
		asOf = req.EvidenceAsOf.UTC().Format(time.RFC3339Nano)
	}
	raw, _ := json.Marshal(struct {
		Contract string    `json:"contract"`
		Ref      string    `json:"ref"`
		Rows     int64     `json:"rows"`
		Hash     string    `json:"hash"`
		AsOf     string    `json:"as_of"`
		Outcomes []outcome `json:"outcomes"`
	}{contractID, req.PopulationRef, req.PopulationRowCount, req.PopulationContentHash, asOf, outs})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
