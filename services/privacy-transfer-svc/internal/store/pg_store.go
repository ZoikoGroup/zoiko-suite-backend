// Package store implements privacy-transfer-svc's persistence.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-transfer-svc/internal/domain"
	"zoiko.io/privacy-transfer-svc/internal/middleware"
)

// Store is the interface the handler depends on.
type Store interface {
	CreateRelationship(ctx context.Context, tenantID string, req domain.CreateProcessorRelationshipRequest, principalID string) (*domain.ProcessorRelationship, error)
	FindRelationship(ctx context.Context, relationshipID string) (*domain.ProcessorRelationship, error)
	ListRelationships(ctx context.Context) ([]domain.ProcessorRelationship, error)
	UpdateRelationshipStatus(ctx context.Context, relationshipID string, status domain.RelationshipStatus) (*domain.ProcessorRelationship, error)

	AttachSubprocessor(ctx context.Context, relationshipID string, req domain.AttachSubprocessorRequest, principalID string) (*domain.Subprocessor, error)
	ListSubprocessors(ctx context.Context, relationshipID string) ([]domain.Subprocessor, error)

	CreateMechanism(ctx context.Context, tenantID string, req domain.CreateTransferMechanismRequest, principalID string) (*domain.TransferMechanism, error)
	FindMechanism(ctx context.Context, mechanismID string) (*domain.TransferMechanism, error)

	RecordAssessment(ctx context.Context, tenantID string, req domain.RecordTransferAssessmentRequest, principalID string) (*domain.TransferAssessment, error)
	FindLatestAssessment(ctx context.Context, relationshipID string) (*domain.TransferAssessment, error)

	RecordDecision(ctx context.Context, tenantID string, d *domain.TransferDecision) error
	FindDecision(ctx context.Context, decisionID string) (*domain.TransferDecision, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", middleware.TenantFromContext(ctx)); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func marshalSlice(s []string) []byte {
	if s == nil {
		s = []string{}
	}
	raw, _ := json.Marshal(s)
	return raw
}

func unmarshalSlice(raw []byte, dest *[]string) {
	if len(raw) == 0 {
		*dest = []string{}
		return
	}
	_ = json.Unmarshal(raw, dest)
}

// ── processor relationships ──────────────────────────────────────────────────

const relationshipColumns = `
	relationship_id, tenant_id, controller_ref, processor_ref, service, processing_instructions,
	purpose_activity_refs, data_categories, subject_classes, contract_evidence_ref, jurisdictions,
	status, created_at, created_by_principal_id`

func scanRelationship(row pgx.Row) (*domain.ProcessorRelationship, error) {
	r := &domain.ProcessorRelationship{}
	var purposeRefsRaw, categoriesRaw, subjectsRaw, jurisdictionsRaw []byte
	err := row.Scan(&r.RelationshipID, &r.TenantID, &r.ControllerRef, &r.ProcessorRef, &r.Service, &r.ProcessingInstructions,
		&purposeRefsRaw, &categoriesRaw, &subjectsRaw, &r.ContractEvidenceRef, &jurisdictionsRaw,
		&r.Status, &r.CreatedAt, &r.CreatedByPrincipalID)
	if err != nil {
		return nil, err
	}
	unmarshalSlice(purposeRefsRaw, &r.PurposeActivityRefs)
	unmarshalSlice(categoriesRaw, &r.DataCategories)
	unmarshalSlice(subjectsRaw, &r.SubjectClasses)
	unmarshalSlice(jurisdictionsRaw, &r.Jurisdictions)
	return r, nil
}

func (s *PgStore) CreateRelationship(ctx context.Context, tenantID string, req domain.CreateProcessorRelationshipRequest, principalID string) (*domain.ProcessorRelationship, error) {
	id := uuid.New().String()
	var r *domain.ProcessorRelationship
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRelationship(tx.QueryRow(ctx, `
			INSERT INTO processor_relationships (`+relationshipColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'ACTIVE', NOW(), $12)
			RETURNING `+relationshipColumns,
			id, strPtrOrNil(tenantID), req.ControllerRef, req.ProcessorRef, req.Service, strPtrOrNil(req.ProcessingInstructions),
			marshalSlice(req.PurposeActivityRefs), marshalSlice(req.DataCategories), marshalSlice(req.SubjectClasses),
			strPtrOrNil(req.ContractEvidenceRef), marshalSlice(req.Jurisdictions), principalID,
		))
		return err
	})
	if err != nil {
		s.log.Error("pg CreateRelationship failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) FindRelationship(ctx context.Context, relationshipID string) (*domain.ProcessorRelationship, error) {
	var r *domain.ProcessorRelationship
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRelationship(tx.QueryRow(ctx, `SELECT `+relationshipColumns+` FROM processor_relationships WHERE relationship_id = $1`, relationshipID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		s.log.Error("pg FindRelationship failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) ListRelationships(ctx context.Context) ([]domain.ProcessorRelationship, error) {
	var out []domain.ProcessorRelationship
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+relationshipColumns+` FROM processor_relationships ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRelationship(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListRelationships failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) UpdateRelationshipStatus(ctx context.Context, relationshipID string, status domain.RelationshipStatus) (*domain.ProcessorRelationship, error) {
	var r *domain.ProcessorRelationship
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRelationship(tx.QueryRow(ctx, `
			UPDATE processor_relationships SET status = $2 WHERE relationship_id = $1
			RETURNING `+relationshipColumns,
			relationshipID, status,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		s.log.Error("pg UpdateRelationshipStatus failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// ── subprocessors ────────────────────────────────────────────────────────────

func (s *PgStore) AttachSubprocessor(ctx context.Context, relationshipID string, req domain.AttachSubprocessorRequest, principalID string) (*domain.Subprocessor, error) {
	id := uuid.New().String()
	var sp domain.Subprocessor
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM processor_relationships WHERE relationship_id = $1`, relationshipID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRelationshipNotFound
			}
			return err
		}

		var locationsRaw, onwardRaw []byte
		if err := tx.QueryRow(ctx, `
			INSERT INTO subprocessors (subprocessor_id, tenant_id, relationship_id, provider_identity, service, purpose,
				data_scope, processing_locations, onward_subprocessors, notification_approval_model, contract_evidence_ref,
				created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), $12)
			RETURNING subprocessor_id, tenant_id, relationship_id, provider_identity, service, purpose, data_scope,
				processing_locations, onward_subprocessors, notification_approval_model, contract_evidence_ref, created_at, created_by_principal_id`,
			id, tenantID, relationshipID, req.ProviderIdentity, req.Service, strPtrOrNil(req.Purpose), strPtrOrNil(req.DataScope),
			marshalSlice(req.ProcessingLocations), marshalSlice(req.OnwardSubprocessors), strPtrOrNil(req.NotificationApprovalModel),
			strPtrOrNil(req.ContractEvidenceRef), principalID,
		).Scan(&sp.SubprocessorID, &sp.TenantID, &sp.RelationshipID, &sp.ProviderIdentity, &sp.Service, &sp.Purpose, &sp.DataScope,
			&locationsRaw, &onwardRaw, &sp.NotificationApprovalModel, &sp.ContractEvidenceRef, &sp.CreatedAt, &sp.CreatedByPrincipalID); err != nil {
			return err
		}
		unmarshalSlice(locationsRaw, &sp.ProcessingLocations)
		unmarshalSlice(onwardRaw, &sp.OnwardSubprocessors)
		return nil
	})
	if errors.Is(err, domain.ErrRelationshipNotFound) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		s.log.Error("pg AttachSubprocessor failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &sp, nil
}

func (s *PgStore) ListSubprocessors(ctx context.Context, relationshipID string) ([]domain.Subprocessor, error) {
	var out []domain.Subprocessor
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT subprocessor_id, tenant_id, relationship_id, provider_identity, service, purpose, data_scope,
				processing_locations, onward_subprocessors, notification_approval_model, contract_evidence_ref, created_at, created_by_principal_id
			FROM subprocessors WHERE relationship_id = $1 ORDER BY created_at ASC`, relationshipID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sp domain.Subprocessor
			var locationsRaw, onwardRaw []byte
			if err := rows.Scan(&sp.SubprocessorID, &sp.TenantID, &sp.RelationshipID, &sp.ProviderIdentity, &sp.Service, &sp.Purpose, &sp.DataScope,
				&locationsRaw, &onwardRaw, &sp.NotificationApprovalModel, &sp.ContractEvidenceRef, &sp.CreatedAt, &sp.CreatedByPrincipalID); err != nil {
				return err
			}
			unmarshalSlice(locationsRaw, &sp.ProcessingLocations)
			unmarshalSlice(onwardRaw, &sp.OnwardSubprocessors)
			out = append(out, sp)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListSubprocessors failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── transfer mechanisms ──────────────────────────────────────────────────────

const mechanismColumns = `mechanism_id, tenant_id, mechanism_type, evidence_ref, conditions, valid_from, valid_until, created_at, created_by_principal_id`

func scanMechanism(row pgx.Row) (*domain.TransferMechanism, error) {
	m := &domain.TransferMechanism{}
	err := row.Scan(&m.MechanismID, &m.TenantID, &m.MechanismType, &m.EvidenceRef, &m.Conditions, &m.ValidFrom, &m.ValidUntil, &m.CreatedAt, &m.CreatedByPrincipalID)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PgStore) CreateMechanism(ctx context.Context, tenantID string, req domain.CreateTransferMechanismRequest, principalID string) (*domain.TransferMechanism, error) {
	id := uuid.New().String()
	validFrom := timeOrNow(req.ValidFrom)
	var m *domain.TransferMechanism
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		m, err = scanMechanism(tx.QueryRow(ctx, `
			INSERT INTO transfer_mechanisms (`+mechanismColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8)
			RETURNING `+mechanismColumns,
			id, strPtrOrNil(tenantID), req.MechanismType, strPtrOrNil(req.EvidenceRef), strPtrOrNil(req.Conditions),
			validFrom, req.ValidUntil, principalID,
		))
		return err
	})
	if err != nil {
		s.log.Error("pg CreateMechanism failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return m, nil
}

func (s *PgStore) FindMechanism(ctx context.Context, mechanismID string) (*domain.TransferMechanism, error) {
	var m *domain.TransferMechanism
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		m, err = scanMechanism(tx.QueryRow(ctx, `SELECT `+mechanismColumns+` FROM transfer_mechanisms WHERE mechanism_id = $1`, mechanismID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMechanismNotFound
	}
	if err != nil {
		s.log.Error("pg FindMechanism failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return m, nil
}

// ── transfer assessments ─────────────────────────────────────────────────────

func (s *PgStore) RecordAssessment(ctx context.Context, tenantID string, req domain.RecordTransferAssessmentRequest, principalID string) (*domain.TransferAssessment, error) {
	id := uuid.New().String()
	var a domain.TransferAssessment
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO transfer_assessments (assessment_id, tenant_id, relationship_id, outcome, reviewer_principal_id, residual_risk, evidence_ref, review_trigger_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
			RETURNING assessment_id, tenant_id, relationship_id, outcome, reviewer_principal_id, residual_risk, evidence_ref, review_trigger_at, created_at`,
			id, strPtrOrNil(tenantID), req.RelationshipID, req.Outcome, principalID, strPtrOrNil(req.ResidualRisk),
			strPtrOrNil(req.EvidenceRef), req.ReviewTriggerAt,
		).Scan(&a.AssessmentID, &a.TenantID, &a.RelationshipID, &a.Outcome, &a.ReviewerPrincipalID, &a.ResidualRisk, &a.EvidenceRef, &a.ReviewTriggerAt, &a.CreatedAt)
	})
	if err != nil {
		s.log.Error("pg RecordAssessment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &a, nil
}

func (s *PgStore) FindLatestAssessment(ctx context.Context, relationshipID string) (*domain.TransferAssessment, error) {
	var a domain.TransferAssessment
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT assessment_id, tenant_id, relationship_id, outcome, reviewer_principal_id, residual_risk, evidence_ref, review_trigger_at, created_at
			FROM transfer_assessments WHERE relationship_id = $1 ORDER BY created_at DESC LIMIT 1`, relationshipID,
		).Scan(&a.AssessmentID, &a.TenantID, &a.RelationshipID, &a.Outcome, &a.ReviewerPrincipalID, &a.ResidualRisk, &a.EvidenceRef, &a.ReviewTriggerAt, &a.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg FindLatestAssessment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &a, nil
}

// ── transfer decisions ───────────────────────────────────────────────────────

func (s *PgStore) RecordDecision(ctx context.Context, tenantID string, d *domain.TransferDecision) error {
	if d.DecisionID == "" {
		d.DecisionID = uuid.New().String()
	}
	reasonRaw := marshalSlice(d.ReasonCodes)
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO transfer_decisions (decision_id, tenant_id, relationship_id, transfer_mechanism_id, destination_jurisdiction,
				assessment_id, result, reason_codes, actor_principal_id, correlation_id, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
			RETURNING decided_at`,
			d.DecisionID, strPtrOrNil(tenantID), d.RelationshipID, d.TransferMechanismID, strPtrOrNil(d.DestinationJurisdiction),
			d.AssessmentID, d.Result, reasonRaw, d.ActorPrincipalID, strPtrOrNil(d.CorrelationID),
		).Scan(&d.DecidedAt)
	})
	if err != nil {
		s.log.Error("pg RecordDecision failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	d.TenantID = strPtrOrNil(tenantID)
	return nil
}

func (s *PgStore) FindDecision(ctx context.Context, decisionID string) (*domain.TransferDecision, error) {
	var d domain.TransferDecision
	var reasonRaw []byte
	var correlationID, destJurisdiction *string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT decision_id, tenant_id, relationship_id, transfer_mechanism_id, destination_jurisdiction, assessment_id,
				result, reason_codes, actor_principal_id, correlation_id, decided_at
			FROM transfer_decisions WHERE decision_id = $1`, decisionID,
		).Scan(&d.DecisionID, &d.TenantID, &d.RelationshipID, &d.TransferMechanismID, &destJurisdiction, &d.AssessmentID,
			&d.Result, &reasonRaw, &d.ActorPrincipalID, &correlationID, &d.DecidedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDecisionNotFound
	}
	if err != nil {
		s.log.Error("pg FindDecision failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	unmarshalSlice(reasonRaw, &d.ReasonCodes)
	if correlationID != nil {
		d.CorrelationID = *correlationID
	}
	if destJurisdiction != nil {
		d.DestinationJurisdiction = *destJurisdiction
	}
	return &d, nil
}

func timeOrNow(t *time.Time) time.Time {
	if t == nil {
		return time.Now().UTC()
	}
	return *t
}
