package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// AI-04 RunDetection and DetectionRun reads.

const resourceTypeRun = "detection_run"

const runColumns = `run_id, tenant_id, model_id, domain_name, model_provider, model_version,
	model_policy_version, model_published_by, feature_set_version,
	review_threshold_bp, max_drift_bp, observed_drift_bp, drift_policy, drift_breached,
	business_context, created_at, created_by`

func scanRun(row pgx.Row) (*domain.DetectionRun, error) {
	var run domain.DetectionRun
	var driftPolicy string
	err := row.Scan(
		&run.RunID, &run.TenantID, &run.ModelID, &run.DomainName, &run.ModelProvider, &run.ModelVersion,
		&run.ModelPolicyVersion, &run.ModelPublishedBy, &run.FeatureSetVersion,
		&run.ReviewThresholdBP, &run.MaxDriftBP, &run.ObservedDriftBP, &driftPolicy, &run.DriftBreached,
		&run.BusinessContext, &run.CreatedAt, &run.CreatedBy,
	)
	if err != nil {
		return nil, err
	}
	run.DriftPolicy = domain.DriftPolicy(driftPolicy)
	return &run, nil
}

func loadRun(ctx context.Context, tx pgx.Tx, tenantID, runID string) (*domain.DetectionRun, error) {
	run, err := scanRun(tx.QueryRow(ctx,
		`SELECT `+runColumns+` FROM detection_runs WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDetectionRunNotFound
	}
	return run, err
}

// loadRunWithSignals loads a run and attaches its signals in creation order.
func loadRunWithSignals(ctx context.Context, tx pgx.Tx, tenantID, runID string) (*domain.DetectionRun, error) {
	run, err := loadRun(ctx, tx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	signals, err := loadSignalsOfRun(ctx, tx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	run.Signals = signals
	return run, nil
}

// RunDetection records one detection run against a registered model.
//
// Order of checks:
//  1. Idempotency-Key replay returns the original run (even if the model
//     changed since).
//  2. The model must be registered (422) and Active (409).
//  3. Drift gate: observed_drift_bp > max_drift_bp applies the model's
//     drift policy — BLOCK refuses, DISABLE refuses and disables the model,
//     ESCALATE accepts but forces every signal to ReviewPending.
//  4. Each signal starts Detected, or ReviewPending when its score reaches
//     the review threshold (or the drift gate escalated the run).
//  5. One AI.AnomalyDetected outbox event per signal, same transaction.
func (s *PgStore) RunDetection(ctx context.Context, cmd domain.CommandContext, req domain.RunDetectionRequest) (domain.CommandResult[domain.DetectionRun], error) {
	var result domain.CommandResult[domain.DetectionRun]
	modelToDisable := ""

	err := s.inTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		runID := newID(domain.PrefixDetectionRun)

		claim, err := claimIdempotencyKey(ctx, tx, cmd, resourceTypeRun, runID)
		if err != nil {
			return err
		}
		if claim.IsReplay {
			original, err := loadRunWithSignals(ctx, tx, cmd.TenantID, claim.OriginalResourceID)
			if err != nil {
				return err
			}
			result = domain.CommandResult[domain.DetectionRun]{Resource: original, Replayed: true}
			return nil
		}

		model, err := loadModelByIdentity(ctx, tx, cmd.TenantID, req.DomainName, req.ModelProvider, req.ModelVersion)
		if err != nil {
			return err
		}
		if model.Status != domain.ModelActive {
			return domain.ErrAnomalyModelDisabled
		}

		observedDrift := *req.ObservedDriftBP
		driftBreached := observedDrift > model.MaxDriftBP
		if driftBreached {
			detail := fmt.Sprintf("observed_drift_bp %d exceeds the model's max_drift_bp %d (drift_policy %s)",
				observedDrift, model.MaxDriftBP, model.DriftPolicy)
			switch model.DriftPolicy {
			case domain.DriftPolicyBlock:
				return domain.ErrDriftExceedsThreshold.WithDetail(detail + "; the run was refused")
			case domain.DriftPolicyDisable:
				modelToDisable = model.ModelID
				return domain.ErrDriftExceedsThreshold.WithDetail(detail + "; the run was refused and the model is now Disabled")
			case domain.DriftPolicyEscalate:
				// Accepted: every signal will be ReviewPending below.
			}
		}

		run, err := scanRun(tx.QueryRow(ctx, `
			INSERT INTO detection_runs
				(run_id, tenant_id, model_id, domain_name, model_provider, model_version,
				 model_policy_version, model_published_by, feature_set_version,
				 review_threshold_bp, max_drift_bp, observed_drift_bp, drift_policy, drift_breached,
				 business_context, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			RETURNING `+runColumns,
			runID, cmd.TenantID, model.ModelID, model.DomainName, model.ModelProvider, model.ModelVersion,
			model.Version, model.UpdatedBy, req.FeatureSetVersion,
			model.ReviewThresholdBP, model.MaxDriftBP, observedDrift, string(model.DriftPolicy), driftBreached,
			req.BusinessContext, cmd.PrincipalID))
		if err != nil {
			return fmt.Errorf("insert detection run: %w", err)
		}

		run.Signals = make([]domain.GovernedAnomalySignal, 0, len(req.Signals))
		for _, input := range req.Signals {
			signal, err := insertSignal(ctx, tx, run, input)
			if err != nil {
				return err
			}
			err = enqueueEvent(ctx, tx, cmd, outboxEvent{
				EventType:     domain.EventAnomalyDetected,
				AggregateType: resourceTypeSignal,
				AggregateID:   signal.SignalID,
				Payload:       signalDetectedPayload(run, signal),
			})
			if err != nil {
				return err
			}
			run.Signals = append(run.Signals, *signal)
		}

		result = domain.CommandResult[domain.DetectionRun]{Resource: run}
		return nil
	})

	// DISABLE policy: the run transaction rolled back, now persist the
	// disable on its own. If that fails too, report it — never pretend.
	if modelToDisable != "" {
		if disableErr := s.disableModelAfterDrift(ctx, cmd.TenantID, modelToDisable, cmd.PrincipalID); disableErr != nil {
			return result, fmt.Errorf("drift policy DISABLE could not disable model %s: %w", modelToDisable, disableErr)
		}
	}
	return result, err
}

// insertSignal stores one signal of a run with model and feature-set
// provenance frozen from the run.
func insertSignal(ctx context.Context, tx pgx.Tx, run *domain.DetectionRun, input domain.GovernedSignalInput) (*domain.GovernedAnomalySignal, error) {
	features := input.Features
	if features == nil {
		features = map[string]string{}
	}
	featuresJSON, err := json.Marshal(features)
	if err != nil {
		return nil, fmt.Errorf("marshal signal features: %w", err)
	}

	score := *input.AnomalyScoreBP
	initialStatus := domain.SignalDetected
	if run.DriftBreached || score >= run.ReviewThresholdBP {
		initialStatus = domain.SignalReviewPending
	}

	contentHash, err := signalContentHash(run, input, features)
	if err != nil {
		return nil, err
	}

	signal, err := scanSignal(tx.QueryRow(ctx, `
		INSERT INTO anomaly_signals
			(signal_id, tenant_id, run_id, domain_name, model_provider, model_version, feature_set_version,
			 source_entity_ref, severity, anomaly_score_bp, features, content_hash, status, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 1)
		RETURNING `+signalColumns,
		newID(domain.PrefixAnomalySignal), run.TenantID, run.RunID, run.DomainName, run.ModelProvider,
		run.ModelVersion, run.FeatureSetVersion, input.SourceEntityRef, string(input.Severity), score,
		featuresJSON, contentHash, string(initialStatus)))
	if err != nil {
		return nil, fmt.Errorf("insert anomaly signal: %w", err)
	}
	return signal, nil
}

// signalContentHash is a SHA-256 over the signal's evidence and provenance,
// so later tampering with any of it is detectable.
func signalContentHash(run *domain.DetectionRun, input domain.GovernedSignalInput, features map[string]string) (string, error) {
	evidence := map[string]any{
		"model_provider":      run.ModelProvider,
		"model_version":       run.ModelVersion,
		"feature_set_version": run.FeatureSetVersion,
		"source_entity_ref":   input.SourceEntityRef,
		"severity":            input.Severity,
		"anomaly_score_bp":    *input.AnomalyScoreBP,
		"features":            features,
	}
	canonicalJSON, err := json.Marshal(evidence) // map keys are sorted by encoding/json
	if err != nil {
		return "", fmt.Errorf("hash signal evidence: %w", err)
	}
	sum := sha256.Sum256(canonicalJSON)
	return hex.EncodeToString(sum[:]), nil
}

// signalDetectedPayload is the AI.AnomalyDetected event body.
func signalDetectedPayload(run *domain.DetectionRun, signal *domain.GovernedAnomalySignal) map[string]any {
	return map[string]any{
		"surface":             "governed",
		"signal_id":           signal.SignalID,
		"run_id":              run.RunID,
		"model_id":            run.ModelID,
		"model_provider":      run.ModelProvider,
		"model_version":       run.ModelVersion,
		"feature_set_version": run.FeatureSetVersion,
		"domain_name":         signal.DomainName,
		"source_entity_ref":   signal.SourceEntityRef,
		"severity":            signal.Severity,
		"anomaly_score_bp":    signal.AnomalyScoreBP,
		"status":              signal.Status,
		"content_hash":        signal.ContentHash,
		"drift_breached":      run.DriftBreached,
		"advisory":            true,
		"assurance_state":     domain.AssuranceStateAdvisory,
	}
}

// GetDetectionRun returns one run with its signals (404 otherwise).
func (s *PgStore) GetDetectionRun(ctx context.Context, tenantID, runID string) (*domain.DetectionRun, error) {
	var run *domain.DetectionRun
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadRunWithSignals(ctx, tx, tenantID, runID)
		run = loaded
		return err
	})
	return run, err
}

// GetSignalsByRun returns the signals of one run. An unknown (or other
// tenant's) run is 404, not an empty list.
func (s *PgStore) GetSignalsByRun(ctx context.Context, tenantID, runID string) ([]domain.GovernedAnomalySignal, error) {
	var signals []domain.GovernedAnomalySignal
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := loadRun(ctx, tx, tenantID, runID); err != nil {
			return err
		}
		loaded, err := loadSignalsOfRun(ctx, tx, tenantID, runID)
		signals = loaded
		return err
	})
	return signals, err
}
