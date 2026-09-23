// BIZ-04 Form Definition & Submission persistence — see
// internal/domain/form.go for the lifecycle/immutability doc comment and
// migration 000010 for the schema/triggers this operates against.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/workflow-svc/internal/domain"
)

const formDefinitionColumns = `form_id, tenant_id, legal_entity_id, name, business_purpose, target_domain, schema,
	owner_principal_id, status, version, published_by_principal_id, published_at, retired_by_principal_id, retired_at, created_at`

func scanFormDefinition(row pgx.Row) (*domain.FormDefinition, error) {
	f := &domain.FormDefinition{}
	var schemaRaw []byte
	err := row.Scan(&f.FormID, &f.TenantID, &f.LegalEntityID, &f.Name, &f.BusinessPurpose, &f.TargetDomain, &schemaRaw,
		&f.OwnerPrincipalID, &f.Status, &f.Version, &f.PublishedByPrincipalID, &f.PublishedAt, &f.RetiredByPrincipalID, &f.RetiredAt, &f.CreatedAt)
	if err != nil {
		return nil, err
	}
	if len(schemaRaw) > 0 {
		if err := json.Unmarshal(schemaRaw, &f.Schema); err != nil {
			return nil, fmt.Errorf("decode schema: %w", err)
		}
	}
	return f, nil
}

const formSubmissionColumns = `submission_id, form_id, tenant_id, legal_entity_id, form_version, status, submitted_values,
	submitter_principal_id, consent_attestation, validation_result, rejection_reason, superseded_by_submission_id,
	created_at, submitted_at, validated_at`

func scanFormSubmission(row pgx.Row) (*domain.FormSubmission, error) {
	s := &domain.FormSubmission{}
	var valuesRaw, validationRaw []byte
	err := row.Scan(&s.SubmissionID, &s.FormID, &s.TenantID, &s.LegalEntityID, &s.FormVersion, &s.Status, &valuesRaw,
		&s.SubmitterPrincipalID, &s.ConsentAttestation, &validationRaw, &s.RejectionReason, &s.SupersededBySubmissionID,
		&s.CreatedAt, &s.SubmittedAt, &s.ValidatedAt)
	if err != nil {
		return nil, err
	}
	if len(valuesRaw) > 0 {
		if err := json.Unmarshal(valuesRaw, &s.SubmittedValues); err != nil {
			return nil, fmt.Errorf("decode submitted_values: %w", err)
		}
	}
	if len(validationRaw) > 0 {
		if err := json.Unmarshal(validationRaw, &s.ValidationResult); err != nil {
			return nil, fmt.Errorf("decode validation_result: %w", err)
		}
	}
	return s, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateForm creates a new DRAFT form definition — BIZ-04's own
// CreateForm command. Idempotent on (tenant_id, correlation_id).
func (s *PgStore) CreateForm(ctx context.Context, p domain.CreateFormParams) (*domain.FormDefinition, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	schemaJSON, err := json.Marshal(p.Schema)
	if err != nil {
		return nil, false, fmt.Errorf("%w: encode schema: %v", domain.ErrStoreUnavailable, err)
	}
	var out *domain.FormDefinition
	created := false
	txErr := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO form_definitions (
				form_id, tenant_id, legal_entity_id, name, business_purpose, target_domain, schema, owner_principal_id, correlation_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)
			ON CONFLICT DO NOTHING RETURNING `+formDefinitionColumns,
			uuid.NewString(), p.TenantID, p.LegalEntityID, p.Name, p.BusinessPurpose, p.TargetDomain, schemaJSON, p.OwnerPrincipalID, p.CorrelationID)
		var err error
		out, err = scanFormDefinition(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanFormDefinition(tx.QueryRow(ctx, `SELECT `+formDefinitionColumns+` FROM form_definitions WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
		return err
	})
	if txErr != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, txErr)
	}
	return out, created, nil
}

func (s *PgStore) GetForm(ctx context.Context, tenantID, formID string) (*domain.FormDefinition, error) {
	var out *domain.FormDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanFormDefinition(tx.QueryRow(ctx, `SELECT `+formDefinitionColumns+` FROM form_definitions WHERE form_id=$1`, formID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFormNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// PublishForm moves a DRAFT form to PUBLISHED — BIZ-04's own PublishForm
// command. Maker-checker is enforced for EVERY form, not only ones
// flagged sensitive (same resolution as BIZ-03's template approval
// scope): the publishing principal can never be the form's own owner
// (also a DB CHECK constraint, migration 000010).
func (s *PgStore) PublishForm(ctx context.Context, p domain.PublishFormParams) (*domain.FormDefinition, error) {
	var out *domain.FormDefinition
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := scanFormDefinition(tx.QueryRow(ctx, `SELECT `+formDefinitionColumns+` FROM form_definitions WHERE form_id=$1 FOR UPDATE`, p.FormID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormNotFound
		}
		if err != nil {
			return err
		}
		if current.OwnerPrincipalID == p.ActorPrincipalID {
			return domain.ErrFormSelfPublish
		}
		if current.Status != "DRAFT" {
			return domain.ErrFormNotDraft
		}
		out, err = scanFormDefinition(tx.QueryRow(ctx, `
			UPDATE form_definitions SET status='PUBLISHED', published_by_principal_id=$2, published_at=now()
			WHERE form_id=$1 AND status='DRAFT' RETURNING `+formDefinitionColumns,
			p.FormID, p.ActorPrincipalID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormNotDraft
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrFormNotFound) || errors.Is(err, domain.ErrFormSelfPublish) || errors.Is(err, domain.ErrFormNotDraft) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// RetireForm retires a form (from DRAFT or PUBLISHED) — BIZ-04's own
// RetireForm command. Filling a real gap in the doc: the command list
// names no explicit "RetireForm", but the lifecycle names a Retired
// state with nothing else that reaches it — same class of gap as
// BNK-09's TreasuryTransferCancelled, flagged and filled rather than
// left unreachable.
func (s *PgStore) RetireForm(ctx context.Context, p domain.RetireFormParams) (*domain.FormDefinition, error) {
	var out *domain.FormDefinition
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := scanFormDefinition(tx.QueryRow(ctx, `SELECT `+formDefinitionColumns+` FROM form_definitions WHERE form_id=$1 FOR UPDATE`, p.FormID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormNotFound
		}
		if err != nil {
			return err
		}
		if current.Status == "RETIRED" {
			return domain.ErrFormAlreadyRetired
		}
		out, err = scanFormDefinition(tx.QueryRow(ctx, `
			UPDATE form_definitions SET status='RETIRED', retired_by_principal_id=$2, retired_at=now()
			WHERE form_id=$1 AND status <> 'RETIRED' RETURNING `+formDefinitionColumns,
			p.FormID, p.ActorPrincipalID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormAlreadyRetired
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrFormNotFound) || errors.Is(err, domain.ErrFormAlreadyRetired) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// SaveDraft creates a submission's first DRAFT (SubmissionID empty) or
// updates an existing DRAFT (SubmissionID set) — BIZ-04's own SaveDraft
// command, mirroring how a real form-filling UI works: repeated partial
// saves before the final SubmitForm. Refuses to create a draft against a
// RETIRED form.
func (s *PgStore) SaveDraft(ctx context.Context, p domain.SaveDraftParams) (*domain.FormSubmission, error) {
	valuesJSON, err := json.Marshal(p.SubmittedValues)
	if err != nil {
		return nil, fmt.Errorf("%w: encode submitted_values: %v", domain.ErrStoreUnavailable, err)
	}
	var out *domain.FormSubmission
	txErr := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		if p.SubmissionID == "" {
			var legalEntityID string
			var formVersion int
			var status string
			if err := tx.QueryRow(ctx, `SELECT legal_entity_id, version, status FROM form_definitions WHERE form_id=$1`, p.FormID).
				Scan(&legalEntityID, &formVersion, &status); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.ErrFormNotFound
				}
				return err
			}
			if status == "RETIRED" {
				return domain.ErrFormRetired
			}
			row := tx.QueryRow(ctx, `INSERT INTO form_submissions (
					submission_id, form_id, tenant_id, legal_entity_id, form_version, submitted_values, submitter_principal_id, consent_attestation, correlation_id
				) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9)
				ON CONFLICT DO NOTHING RETURNING `+formSubmissionColumns,
				uuid.NewString(), p.FormID, p.TenantID, legalEntityID, formVersion, valuesJSON, p.SubmitterPrincipalID, nullIfEmpty(p.ConsentAttestation), nullIfEmpty(p.CorrelationID))
			out, err = scanFormSubmission(row)
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			out, err = scanFormSubmission(tx.QueryRow(ctx, `SELECT `+formSubmissionColumns+` FROM form_submissions WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
			return err
		}

		current, err := scanFormSubmission(tx.QueryRow(ctx, `SELECT `+formSubmissionColumns+` FROM form_submissions WHERE submission_id=$1 FOR UPDATE`, p.SubmissionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormSubmissionNotFound
		}
		if err != nil {
			return err
		}
		if current.Status != domain.FormSubmissionDraft {
			return domain.ErrFormSubmissionNotDraft
		}
		out, err = scanFormSubmission(tx.QueryRow(ctx, `
			UPDATE form_submissions SET submitted_values=$2::jsonb, consent_attestation=$3
			WHERE submission_id=$1 AND status='DRAFT' RETURNING `+formSubmissionColumns,
			p.SubmissionID, valuesJSON, nullIfEmpty(p.ConsentAttestation)))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormSubmissionNotDraft
		}
		return err
	})
	if txErr != nil {
		if errors.Is(txErr, domain.ErrFormNotFound) || errors.Is(txErr, domain.ErrFormRetired) ||
			errors.Is(txErr, domain.ErrFormSubmissionNotFound) || errors.Is(txErr, domain.ErrFormSubmissionNotDraft) {
			return nil, txErr
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, txErr)
	}
	return out, nil
}

// SubmitForm moves a DRAFT submission to SUBMITTED — BIZ-04's own
// SubmitForm command. From this point the submitted values are
// immutable (migration 000010's own trigger), the doc's own "immutable
// submissions" purpose line.
func (s *PgStore) SubmitForm(ctx context.Context, p domain.SubmitFormParams) (*domain.FormSubmission, error) {
	var out *domain.FormSubmission
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := scanFormSubmission(tx.QueryRow(ctx, `SELECT `+formSubmissionColumns+` FROM form_submissions WHERE submission_id=$1 FOR UPDATE`, p.SubmissionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormSubmissionNotFound
		}
		if err != nil {
			return err
		}
		if current.Status != domain.FormSubmissionDraft {
			return domain.ErrFormSubmissionNotDraft
		}
		out, err = scanFormSubmission(tx.QueryRow(ctx, `
			UPDATE form_submissions SET status='SUBMITTED', submitted_at=now()
			WHERE submission_id=$1 AND status='DRAFT' RETURNING `+formSubmissionColumns,
			p.SubmissionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormSubmissionNotDraft
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrFormSubmissionNotFound) || errors.Is(err, domain.ErrFormSubmissionNotDraft) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ValidateSubmission checks a SUBMITTED submission's values against its
// form's required-field schema — BIZ-04's own ValidateSubmission
// command — and concludes directly to ACCEPTED or REJECTED. VALIDATING
// is a real named lifecycle state, but there is no second command to
// conclude it from: the doc names exactly one ValidateSubmission
// command, so validation here is synchronous and the persisted result is
// always a terminal-for-this-round outcome, never a submission left
// sitting in VALIDATING.
func (s *PgStore) ValidateSubmission(ctx context.Context, p domain.ValidateSubmissionParams) (*domain.FormSubmission, error) {
	var out *domain.FormSubmission
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := scanFormSubmission(tx.QueryRow(ctx, `SELECT `+formSubmissionColumns+` FROM form_submissions WHERE submission_id=$1 FOR UPDATE`, p.SubmissionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormSubmissionNotFound
		}
		if err != nil {
			return err
		}
		if current.Status != domain.FormSubmissionSubmitted {
			return domain.ErrFormSubmissionNotSubmitted
		}

		var schemaRaw []byte
		if err := tx.QueryRow(ctx, `SELECT schema FROM form_definitions WHERE form_id=$1`, current.FormID).Scan(&schemaRaw); err != nil {
			return err
		}
		var schema []string
		if len(schemaRaw) > 0 {
			if err := json.Unmarshal(schemaRaw, &schema); err != nil {
				return fmt.Errorf("decode schema: %w", err)
			}
		}
		var missing []string
		for _, field := range schema {
			if current.SubmittedValues[field] == "" {
				missing = append(missing, field)
			}
		}
		result := domain.ValidationResult{Passed: len(missing) == 0, Missing: missing}
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode validation_result: %w", err)
		}

		newStatus := domain.FormSubmissionAccepted
		var rejectionReason any
		if !result.Passed {
			newStatus = domain.FormSubmissionRejected
			rejectionReason = fmt.Sprintf("missing required fields: %v", missing)
		}
		out, err = scanFormSubmission(tx.QueryRow(ctx, `
			UPDATE form_submissions SET status=$2, validation_result=$3::jsonb, rejection_reason=$4, validated_at=now()
			WHERE submission_id=$1 AND status='SUBMITTED' RETURNING `+formSubmissionColumns,
			p.SubmissionID, newStatus, resultJSON, rejectionReason))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormSubmissionNotSubmitted
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrFormSubmissionNotFound) || errors.Is(err, domain.ErrFormSubmissionNotSubmitted) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) GetSubmission(ctx context.Context, tenantID, submissionID string) (*domain.FormSubmission, error) {
	var out *domain.FormSubmission
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanFormSubmission(tx.QueryRow(ctx, `SELECT `+formSubmissionColumns+` FROM form_submissions WHERE submission_id=$1`, submissionID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFormSubmissionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
