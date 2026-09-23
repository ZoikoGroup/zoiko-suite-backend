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

// SupersedeSubmission confirms a corrected resubmission and, in the same
// transaction, marks the submission it replaces as SUPERSEDED —
// BIZ-04's own SupersedeSubmission command. Forward-links via
// superseded_by_submission_id, never overwriting the previous row's own
// facts — mirrors document-vault-svc's SupersedeClassification. The
// replacement must itself be ACCEPTED: a still-pending or still-rejected
// resubmission cannot yet stand in for the one it would replace.
func (s *PgStore) SupersedeSubmission(ctx context.Context, p domain.SupersedeSubmissionParams) (*domain.FormSubmission, error) {
	var out *domain.FormSubmission
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		previous, err := scanFormSubmission(tx.QueryRow(ctx, `SELECT `+formSubmissionColumns+` FROM form_submissions WHERE submission_id=$1 FOR UPDATE`, p.PreviousSubmissionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormSubmissionNotFound
		}
		if err != nil {
			return err
		}
		// Checked in this order deliberately: a SUPERSEDED row already has
		// superseded_by_submission_id set, so checking that first reports
		// the precise reason (already superseded) instead of the more
		// generic "not accepted or rejected" a status check alone would
		// give it — same class of ordering bug found and fixed in
		// document-vault-svc's SupersedeClassification earlier this build.
		if previous.SupersededBySubmissionID != nil {
			return domain.ErrFormSubmissionAlreadySuperseded
		}
		if previous.Status != domain.FormSubmissionAccepted && previous.Status != domain.FormSubmissionRejected {
			return domain.ErrFormSubmissionNotAcceptedOrRejected
		}

		next, err := scanFormSubmission(tx.QueryRow(ctx, `SELECT `+formSubmissionColumns+` FROM form_submissions WHERE submission_id=$1`, p.NewSubmissionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFormSubmissionNotFound
		}
		if err != nil {
			return err
		}
		if next.FormID != previous.FormID {
			return domain.ErrFormSubmissionsBelongToDifferentForms
		}
		if next.Status != domain.FormSubmissionAccepted {
			return domain.ErrFormSubmissionNotAccepted
		}

		tag, err := tx.Exec(ctx, `
			UPDATE form_submissions SET status='SUPERSEDED', superseded_by_submission_id=$2
			WHERE submission_id=$1 AND superseded_by_submission_id IS NULL`,
			p.PreviousSubmissionID, p.NewSubmissionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrFormSubmissionAlreadySuperseded
		}
		out = next
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrFormSubmissionNotFound) || errors.Is(err, domain.ErrFormSubmissionNotAcceptedOrRejected) ||
			errors.Is(err, domain.ErrFormSubmissionAlreadySuperseded) || errors.Is(err, domain.ErrFormSubmissionsBelongToDifferentForms) ||
			errors.Is(err, domain.ErrFormSubmissionNotAccepted) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

const formSubmissionRouteColumns = `route_id, submission_id, tenant_id, target_domain, command_reference, result_reference,
	outcome, failure_reason, routed_by_principal_id, routed_at`

func scanFormSubmissionRoute(row pgx.Row) (*domain.FormSubmissionRoute, error) {
	r := &domain.FormSubmissionRoute{}
	err := row.Scan(&r.RouteID, &r.SubmissionID, &r.TenantID, &r.TargetDomain, &r.CommandReference, &r.ResultReference,
		&r.Outcome, &r.FailureReason, &r.RoutedByPrincipalID, &r.RoutedAt)
	return r, err
}

// RouteToDomain records one downstream routing attempt's evidence —
// BIZ-04's own RouteToDomain command. Deliberately does NOT change
// form_submissions.status: per the doc's own failure semantics, "target
// domain rejection leaves submission accepted as evidence but not as
// successful business action" — the submission stays ACCEPTED forever
// once accepted, and this table is the append-only record of whether it
// was ever successfully routed, and to what. Refuses to route anything
// that is not itself ACCEPTED.
func (s *PgStore) RouteToDomain(ctx context.Context, p domain.RouteToDomainParams) (*domain.FormSubmissionRoute, error) {
	var out *domain.FormSubmissionRoute
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		// target_domain is not caller-supplied: it is the form's own
		// fixed target_domain (set once at CreateForm), joined here rather
		// than trusted from the request, so a route can never claim to
		// have gone somewhere other than where its form was actually
		// defined to go.
		var status, targetDomain string
		if err := tx.QueryRow(ctx, `
			SELECT fs.status, fd.target_domain
			FROM form_submissions fs JOIN form_definitions fd ON fd.form_id = fs.form_id
			WHERE fs.submission_id=$1`, p.SubmissionID).Scan(&status, &targetDomain); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrFormSubmissionNotFound
			}
			return err
		}
		if status != domain.FormSubmissionAccepted {
			return domain.ErrFormSubmissionNotAccepted
		}
		var err error
		out, err = scanFormSubmissionRoute(tx.QueryRow(ctx, `INSERT INTO form_submission_routes (
				route_id, submission_id, tenant_id, target_domain, command_reference, result_reference,
				outcome, failure_reason, routed_by_principal_id, correlation_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+formSubmissionRouteColumns,
			uuid.NewString(), p.SubmissionID, p.TenantID, targetDomain, nullIfEmpty(p.CommandReference), nullIfEmpty(p.ResultReference),
			p.Outcome, nullIfEmpty(p.FailureReason), p.ActorPrincipalID, nullIfEmpty(p.CorrelationID)))
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrFormSubmissionNotFound) || errors.Is(err, domain.ErrFormSubmissionNotAccepted) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// GetSubmissionVersion — BIZ-04's own GetSubmissionVersion query. See
// domain.FormSubmissionVersionInfo's own doc comment for why this is
// narrower than GetSubmission.
func (s *PgStore) GetSubmissionVersion(ctx context.Context, tenantID, submissionID string) (*domain.FormSubmissionVersionInfo, error) {
	var out domain.FormSubmissionVersionInfo
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT submission_id, form_id, form_version FROM form_submissions WHERE submission_id=$1`, submissionID).
			Scan(&out.SubmissionID, &out.FormID, &out.FormVersion)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFormSubmissionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &out, nil
}

// GetValidationResult — BIZ-04's own GetValidationResult query.
func (s *PgStore) GetValidationResult(ctx context.Context, tenantID, submissionID string) (*domain.ValidationResult, error) {
	sub, err := s.GetSubmission(ctx, tenantID, submissionID)
	if err != nil {
		return nil, err
	}
	if sub.ValidationResult == nil {
		return nil, domain.ErrFormSubmissionNotYetValidated
	}
	return sub.ValidationResult, nil
}

// ListPendingSubmissions returns a form's submissions still awaiting a
// validation outcome (SUBMITTED or VALIDATING) — BIZ-04's own
// ListPendingSubmissions query, the governance backlog a
// reviewer/validator works through. Scoped to one form (not the whole
// tenant) so authorization stays meaningful: a caller is authorized per
// legal entity, and a form is the unit that carries one.
func (s *PgStore) ListPendingSubmissions(ctx context.Context, tenantID, formID string, limit, offset int) ([]*domain.FormSubmission, error) {
	var out []*domain.FormSubmission
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+formSubmissionColumns+` FROM form_submissions
			WHERE form_id=$1 AND status IN ('SUBMITTED', 'VALIDATING')
			ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
			formID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sub, err := scanFormSubmission(rows)
			if err != nil {
				return err
			}
			out = append(out, sub)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
