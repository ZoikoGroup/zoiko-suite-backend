package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/middleware"
)

type Store interface {
	Detect(ctx context.Context, rec *domain.AnomalyRecord) error
	GetByID(ctx context.Context, id string) (*domain.AnomalyRecord, error)
	ListAnomalies(ctx context.Context, legalEntityID, domainName, severity, status string) ([]domain.AnomalyRecord, error)
	UpdateStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.AnomalyRecord, error)
	CreateRule(ctx context.Context, rule *domain.AnomalyRule) error
	ListRules(ctx context.Context, domainName string) ([]domain.AnomalyRule, error)

	// AI-04 governed advisory layer (additive; see domain/types.go).
	RegisterAnomalyModel(ctx context.Context, tenantID string, req domain.RegisterAnomalyModelRequest, actor string) (*domain.AnomalyModel, error)
	RunDetection(ctx context.Context, tenantID string, req domain.RunDetectionRequest, actor string, claim domain.IdempotencyClaim) (*domain.DetectionRun, error)
	AcknowledgeSignal(ctx context.Context, tenantID, signalID string, actor string, claim domain.IdempotencyClaim) (*domain.GovernedAnomalySignal, error)
	EscalateForReview(ctx context.Context, tenantID, signalID string, req domain.EscalateForReviewRequest, actor string, claim domain.IdempotencyClaim) (*domain.GovernedAnomalySignal, error)
	CloseSignal(ctx context.Context, tenantID, signalID string, req domain.CloseSignalRequest, actor string, claim domain.IdempotencyClaim) (*domain.GovernedAnomalySignal, error)
	GetGovernedSignal(ctx context.Context, tenantID, signalID string) (*domain.GovernedAnomalySignal, error)
	GetSignalsByRun(ctx context.Context, tenantID, runID string) ([]domain.GovernedAnomalySignal, error)
	GetDisposition(ctx context.Context, tenantID, signalID string) (*domain.ReviewDisposition, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

func (s *PgStore) Detect(ctx context.Context, rec *domain.AnomalyRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if rec.AnomalyID == "" {
		rec.AnomalyID = "anom-" + uuid.New().String()
	}
	rec.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	rec.DetectedAt = now
	rec.CreatedAt = now
	rec.UpdatedAt = now
	if rec.Status == "" {
		rec.Status = domain.StatusOpen
	}

	var ruleID *string
	if rec.RuleID != "" {
		ruleID = &rec.RuleID
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO anomaly_records
			(anomaly_id, tenant_id, legal_entity_id, domain_name, source_entity_id,
			 rule_id, severity, anomaly_score, observed_value, expected_value,
			 description, status, detected_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		rec.AnomalyID, rec.TenantID, rec.LegalEntityID, rec.DomainName, rec.SourceEntityID,
		ruleID, string(rec.Severity), rec.AnomalyScore, rec.ObservedValue, rec.ExpectedValue,
		rec.Description, string(rec.Status), rec.DetectedAt, rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert anomaly record: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetByID(ctx context.Context, id string) (*domain.AnomalyRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var rec domain.AnomalyRecord
	var severityStr, statusStr string
	var ruleID *string

	err = tx.QueryRow(ctx, `
		SELECT anomaly_id, tenant_id, legal_entity_id, domain_name, source_entity_id,
		       rule_id, severity, anomaly_score, observed_value, expected_value,
		       description, status, investigated_by, investigated_at, resolution_notes,
		       detected_at, created_at, updated_at
		FROM anomaly_records WHERE anomaly_id = $1`, id,
	).Scan(
		&rec.AnomalyID, &rec.TenantID, &rec.LegalEntityID, &rec.DomainName, &rec.SourceEntityID,
		&ruleID, &severityStr, &rec.AnomalyScore, &rec.ObservedValue, &rec.ExpectedValue,
		&rec.Description, &statusStr, &rec.InvestigatedBy, &rec.InvestigatedAt, &rec.ResolutionNotes,
		&rec.DetectedAt, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrAnomalyRecordNotFound
		}
		return nil, err
	}

	if ruleID != nil {
		rec.RuleID = *ruleID
	}
	rec.Severity = domain.Severity(severityStr)
	rec.Status = domain.AnomalyStatus(statusStr)
	_ = tx.Commit(ctx)
	return &rec, nil
}

func (s *PgStore) ListAnomalies(ctx context.Context, legalEntityID, domainName, severity, status string) ([]domain.AnomalyRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT anomaly_id, tenant_id, legal_entity_id, domain_name, source_entity_id,
		       rule_id, severity, anomaly_score, observed_value, expected_value,
		       description, status, investigated_by, investigated_at, resolution_notes,
		       detected_at, created_at, updated_at
		FROM anomaly_records
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR domain_name = $3)
		  AND ($3 = '' OR severity = $3)
		  AND ($4 = '' OR status = $4)
		ORDER BY anomaly_score DESC, detected_at DESC`,
		legalEntityID, domainName, severity, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AnomalyRecord
	for rows.Next() {
		var rec domain.AnomalyRecord
		var severityStr, statusStr string
		var ruleID *string

		if err := rows.Scan(
			&rec.AnomalyID, &rec.TenantID, &rec.LegalEntityID, &rec.DomainName, &rec.SourceEntityID,
			&ruleID, &severityStr, &rec.AnomalyScore, &rec.ObservedValue, &rec.ExpectedValue,
			&rec.Description, &statusStr, &rec.InvestigatedBy, &rec.InvestigatedAt, &rec.ResolutionNotes,
			&rec.DetectedAt, &rec.CreatedAt, &rec.UpdatedAt,
		); err != nil {
			return nil, err
		}

		if ruleID != nil {
			rec.RuleID = *ruleID
		}
		rec.Severity = domain.Severity(severityStr)
		rec.Status = domain.AnomalyStatus(statusStr)
		out = append(out, rec)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.AnomalyRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rec, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	rec.Status = req.Status
	rec.InvestigatedBy = req.InvestigatedBy
	rec.InvestigatedAt = &now
	if req.ResolutionNotes != "" {
		rec.ResolutionNotes = req.ResolutionNotes
	}
	rec.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE anomaly_records
		SET status = $1, investigated_by = $2, investigated_at = $3, resolution_notes = $4, updated_at = $5
		WHERE anomaly_id = $6`,
		string(rec.Status), rec.InvestigatedBy, rec.InvestigatedAt, rec.ResolutionNotes, rec.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update anomaly status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *PgStore) CreateRule(ctx context.Context, rule *domain.AnomalyRule) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if rule.RuleID == "" {
		rule.RuleID = "arule-" + uuid.New().String()
	}
	rule.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	rule.CreatedAt = now
	rule.UpdatedAt = now
	rule.IsActive = true

	_, err = tx.Exec(ctx, `
		INSERT INTO anomaly_detection_rules
			(rule_id, tenant_id, rule_name, domain_name, metric_type, threshold_value, z_score_cutoff, is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		rule.RuleID, rule.TenantID, rule.RuleName, rule.DomainName, rule.MetricType,
		rule.ThresholdValue, rule.ZScoreCutoff, rule.IsActive, rule.CreatedAt, rule.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert anomaly rule: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) ListRules(ctx context.Context, domainName string) ([]domain.AnomalyRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT rule_id, tenant_id, rule_name, domain_name, metric_type, threshold_value, z_score_cutoff, is_active, created_at, updated_at
		FROM anomaly_detection_rules
		WHERE ($1 = '' OR domain_name = $1)
		ORDER BY created_at DESC`, domainName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AnomalyRule
	for rows.Next() {
		var r domain.AnomalyRule
		if err := rows.Scan(
			&r.RuleID, &r.TenantID, &r.RuleName, &r.DomainName, &r.MetricType,
			&r.ThresholdValue, &r.ZScoreCutoff, &r.IsActive, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// AI-04 governed advisory layer — additive, does not touch the legacy
// Detect/UpdateStatus/CreateRule methods above. Every method runs
// inside one transaction that first declares app.tenant_id for RLS.
// ─────────────────────────────────────────────────────────────────────────────

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapGovernedErr(err)
	}
	return tx.Commit(ctx)
}

func mapGovernedErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	return fmt.Errorf("anomaly-detection-svc: %s", pgErr.Message)
}

func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES (current_setting('app.tenant_id', true), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = current_setting('app.tenant_id', true) AND owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

func newGovernedID(prefix string) string {
	return prefix + uuid.NewString()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const anomalyModelColumns = `model_id, tenant_id, domain_name, model_provider, model_version, review_threshold_bp, max_drift_bp, created_at, created_by`

func scanAnomalyModel(row pgx.Row) (*domain.AnomalyModel, error) {
	var m domain.AnomalyModel
	if err := row.Scan(&m.ModelID, &m.TenantID, &m.DomainName, &m.ModelProvider, &m.ModelVersion, &m.ReviewThresholdBP,
		&m.MaxDriftBP, &m.CreatedAt, &m.CreatedBy); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *PgStore) RegisterAnomalyModel(ctx context.Context, tenantID string, req domain.RegisterAnomalyModelRequest, actor string) (*domain.AnomalyModel, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.AnomalyModel
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		id := newGovernedID(domain.PrefixAnomalyModel)
		got, err := scanAnomalyModel(tx.QueryRow(ctx, `
			INSERT INTO anomaly_models (model_id, tenant_id, domain_name, model_provider, model_version, review_threshold_bp, max_drift_bp, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant_id, domain_name, model_provider, model_version)
				DO UPDATE SET review_threshold_bp = EXCLUDED.review_threshold_bp, max_drift_bp = EXCLUDED.max_drift_bp
			RETURNING `+anomalyModelColumns,
			id, tenantID, req.DomainName, req.ModelProvider, req.ModelVersion, req.ReviewThresholdBP, req.MaxDriftBP, actor))
		if err != nil {
			return fmt.Errorf("register anomaly model: %w", err)
		}
		out = got
		return nil
	})
	return out, err
}

func loadAnomalyModel(ctx context.Context, tx pgx.Tx, tenantID, domainName, provider, version string) (*domain.AnomalyModel, error) {
	m, err := scanAnomalyModel(tx.QueryRow(ctx, `SELECT `+anomalyModelColumns+` FROM anomaly_models
		WHERE tenant_id = $1 AND domain_name = $2 AND model_provider = $3 AND model_version = $4`,
		tenantID, domainName, provider, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAnomalyModelNotFound
	}
	return m, err
}

// RunDetection refuses outright, before anything is created, when the
// invocation's own observed drift exceeds the registered anomaly
// model's maximum. The idempotency claim is taken before the drift
// check so a replay of an already-succeeded call never fails because
// the registry changed in the meantime — it always returns the
// original result.
func (s *PgStore) RunDetection(ctx context.Context, tenantID string, req domain.RunDetectionRequest, actor string, claim domain.IdempotencyClaim) (*domain.DetectionRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DetectionRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		runID := newGovernedID(domain.PrefixDetectionRun)
		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		model, err := loadAnomalyModel(ctx, tx, tenantID, req.DomainName, req.ModelProvider, req.ModelVersion)
		if err != nil {
			return err
		}
		if req.ObservedDriftBP > model.MaxDriftBP {
			return domain.ErrDriftExceedsThreshold
		}

		var run domain.DetectionRun
		if err := tx.QueryRow(ctx, `
			INSERT INTO detection_runs (run_id, tenant_id, model_id, domain_name, model_provider, model_version, observed_drift_bp, business_context, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING run_id, tenant_id, model_id, domain_name, model_provider, model_version, observed_drift_bp, business_context, created_at, created_by`,
			runID, tenantID, model.ModelID, req.DomainName, req.ModelProvider, req.ModelVersion, req.ObservedDriftBP, req.BusinessContext, actor).
			Scan(&run.RunID, &run.TenantID, &run.ModelID, &run.DomainName, &run.ModelProvider, &run.ModelVersion,
				&run.ObservedDriftBP, &run.BusinessContext, &run.CreatedAt, &run.CreatedBy); err != nil {
			return fmt.Errorf("create detection run: %w", err)
		}

		for _, sig := range req.Signals {
			featuresJSON, err := json.Marshal(sig.Features)
			if err != nil {
				return fmt.Errorf("marshal signal features: %w", err)
			}
			status := domain.SignalDetected
			if sig.AnomalyScoreBP >= model.ReviewThresholdBP {
				status = domain.SignalReviewPending
			}
			signalID := newGovernedID(domain.PrefixAnomalySignal)
			if _, err := tx.Exec(ctx, `
				INSERT INTO anomaly_signals (signal_id, tenant_id, run_id, domain_name, model_provider, model_version,
					source_entity_ref, severity, anomaly_score_bp, features, content_hash, status)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
				signalID, tenantID, runID, req.DomainName, req.ModelProvider, req.ModelVersion,
				sig.SourceEntityRef, sig.Severity, sig.AnomalyScoreBP, featuresJSON, sha256Hex(featuresJSON), status); err != nil {
				return fmt.Errorf("insert anomaly signal: %w", err)
			}
		}

		out = &run
		return nil
	})
	return out, err
}

const governedSignalColumns = `signal_id, tenant_id, run_id, domain_name, model_provider, model_version, source_entity_ref,
	severity, anomaly_score_bp, features, content_hash, status, created_at`

func scanGovernedSignal(row pgx.Row) (*domain.GovernedAnomalySignal, error) {
	var sig domain.GovernedAnomalySignal
	var featuresJSON []byte
	if err := row.Scan(&sig.SignalID, &sig.TenantID, &sig.RunID, &sig.DomainName, &sig.ModelProvider, &sig.ModelVersion,
		&sig.SourceEntityRef, &sig.Severity, &sig.AnomalyScoreBP, &featuresJSON, &sig.ContentHash, &sig.Status, &sig.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(featuresJSON, &sig.Features); err != nil {
		return nil, fmt.Errorf("unmarshal signal features: %w", err)
	}
	return &sig, nil
}

func loadGovernedSignalForUpdate(ctx context.Context, tx pgx.Tx, tenantID, signalID string) (*domain.GovernedAnomalySignal, error) {
	sig, err := scanGovernedSignal(tx.QueryRow(ctx, `SELECT `+governedSignalColumns+` FROM anomaly_signals WHERE tenant_id = $1 AND signal_id = $2 FOR UPDATE`,
		tenantID, signalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSignalNotFound
	}
	return sig, err
}

func (s *PgStore) GetGovernedSignal(ctx context.Context, tenantID, signalID string) (*domain.GovernedAnomalySignal, error) {
	var out *domain.GovernedAnomalySignal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sig, err := scanGovernedSignal(tx.QueryRow(ctx, `SELECT `+governedSignalColumns+` FROM anomaly_signals WHERE tenant_id = $1 AND signal_id = $2`, tenantID, signalID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSignalNotFound
		}
		out = sig
		return err
	})
	return out, err
}

func (s *PgStore) GetSignalsByRun(ctx context.Context, tenantID, runID string) ([]domain.GovernedAnomalySignal, error) {
	var out []domain.GovernedAnomalySignal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+governedSignalColumns+` FROM anomaly_signals WHERE tenant_id = $1 AND run_id = $2 ORDER BY created_at`,
			tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sig, err := scanGovernedSignal(rows)
			if err != nil {
				return err
			}
			out = append(out, *sig)
		}
		return rows.Err()
	})
	return out, err
}

// AcknowledgeSignal moves a freshly Detected signal into ReviewPending
// — someone has started looking at it.
func (s *PgStore) AcknowledgeSignal(ctx context.Context, tenantID, signalID string, actor string, claim domain.IdempotencyClaim) (*domain.GovernedAnomalySignal, error) {
	var out *domain.GovernedAnomalySignal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sig, err := loadGovernedSignalForUpdate(ctx, tx, tenantID, signalID)
		if err != nil {
			return err
		}
		claim.ResourceID = signalID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if sig.Status != domain.SignalDetected {
			return domain.ErrSignalNotDetected
		}
		updated, err := scanGovernedSignal(tx.QueryRow(ctx, `UPDATE anomaly_signals SET status = 'ReviewPending' WHERE signal_id = $1 RETURNING `+governedSignalColumns, signalID))
		if err != nil {
			return fmt.Errorf("acknowledge signal: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

// EscalateForReview moves a ReviewPending signal to Escalated — a
// human decided this needs attention beyond ordinary disposition.
func (s *PgStore) EscalateForReview(ctx context.Context, tenantID, signalID string, req domain.EscalateForReviewRequest, actor string, claim domain.IdempotencyClaim) (*domain.GovernedAnomalySignal, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.GovernedAnomalySignal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sig, err := loadGovernedSignalForUpdate(ctx, tx, tenantID, signalID)
		if err != nil {
			return err
		}
		claim.ResourceID = signalID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if sig.Status != domain.SignalReviewPending {
			return domain.ErrSignalNotReviewable
		}
		updated, err := scanGovernedSignal(tx.QueryRow(ctx, `UPDATE anomaly_signals SET status = 'Escalated' WHERE signal_id = $1 RETURNING `+governedSignalColumns, signalID))
		if err != nil {
			return fmt.Errorf("escalate signal: %w", err)
		}
		if err := recordDisposition(ctx, tx, tenantID, signalID, domain.SignalEscalated, req.Reason, actor); err != nil {
			return err
		}
		out = updated
		return nil
	})
	return out, err
}

// CloseSignal records the human terminal disposition — Confirmed or
// Dismissed — as evidence. It writes nothing beyond this layer's own
// review_dispositions row: no payment, write-off, or freeze action is
// ever triggered here, regardless of outcome or severity.
func (s *PgStore) CloseSignal(ctx context.Context, tenantID, signalID string, req domain.CloseSignalRequest, actor string, claim domain.IdempotencyClaim) (*domain.GovernedAnomalySignal, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.GovernedAnomalySignal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sig, err := loadGovernedSignalForUpdate(ctx, tx, tenantID, signalID)
		if err != nil {
			return err
		}
		claim.ResourceID = signalID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if sig.Status != domain.SignalReviewPending {
			return domain.ErrSignalNotReviewable
		}
		updated, err := scanGovernedSignal(tx.QueryRow(ctx, `UPDATE anomaly_signals SET status = $2 WHERE signal_id = $1 RETURNING `+governedSignalColumns,
			signalID, req.Outcome))
		if err != nil {
			return fmt.Errorf("close signal: %w", err)
		}
		if err := recordDisposition(ctx, tx, tenantID, signalID, req.Outcome, req.Notes, actor); err != nil {
			return err
		}
		out = updated
		return nil
	})
	return out, err
}

func recordDisposition(ctx context.Context, tx pgx.Tx, tenantID, signalID string, outcome domain.GovernedSignalStatus, notes, actor string) error {
	dispID := newGovernedID(domain.PrefixDisposition)
	_, err := tx.Exec(ctx, `
		INSERT INTO review_dispositions (disposition_id, tenant_id, signal_id, outcome, notes, actor)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		dispID, tenantID, signalID, outcome, notes, actor)
	if err != nil {
		return fmt.Errorf("record review disposition: %w", err)
	}
	return nil
}

const dispositionColumns = `disposition_id, tenant_id, signal_id, outcome, notes, actor, decided_at`

func (s *PgStore) GetDisposition(ctx context.Context, tenantID, signalID string) (*domain.ReviewDisposition, error) {
	var out *domain.ReviewDisposition
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var d domain.ReviewDisposition
		err := tx.QueryRow(ctx, `SELECT `+dispositionColumns+` FROM review_dispositions WHERE tenant_id = $1 AND signal_id = $2`, tenantID, signalID).
			Scan(&d.DispositionID, &d.TenantID, &d.SignalID, &d.Outcome, &d.Notes, &d.Actor, &d.DecidedAt)
		if err != nil {
			return err
		}
		out = &d
		return nil
	})
	return out, err
}
