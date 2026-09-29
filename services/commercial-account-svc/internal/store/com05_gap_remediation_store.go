// COM-05 gap-remediation persistence: D1 (CommercialEvidencePackage query —
// the write path lives inside IssueInvoice, com05_billing_store.go, since
// the package is sealed in the same transaction as the invoice itself), D2
// (registered tax jurisdictions), D3 (the shared accounting-event emission
// helper), and D4 (dispute/chargeback tracking).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/outbox"
)

// ── D1: evidence package query ──────────────────────────────────────────────

const evidencePackageColumns = `package_id, invoice_id, organization_id::text, manifest, manifest_sha256,
	sealed_at, sealed_by_principal_id`

func scanEvidencePackage(row pgx.Row) (*domain.CommercialEvidencePackage, error) {
	var p domain.CommercialEvidencePackage
	var manifestJSON []byte
	if err := row.Scan(&p.PackageID, &p.InvoiceID, &p.OrganizationID, &manifestJSON, &p.ManifestSHA256,
		&p.SealedAt, &p.SealedByPrincipalID); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(manifestJSON, &p.Manifest); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PgStore) GetEvidencePackage(ctx context.Context, invoiceID string) (*domain.CommercialEvidencePackage, error) {
	var out *domain.CommercialEvidencePackage
	err := s.billingTx(ctx, func(tx pgx.Tx) error {
		p, err := scanEvidencePackage(tx.QueryRow(ctx, `SELECT `+evidencePackageColumns+`
			FROM commercial_evidence_packages WHERE invoice_id = $1`, invoiceID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrEvidencePackageNotFound
		}
		out = p
		return err
	})
	return out, err
}

// ── D2: registered tax jurisdictions ────────────────────────────────────────

const taxJurisdictionColumns = `billing_account_id, jurisdiction_code, registered_rate_basis_points,
	effective_from, created_at, created_by_principal_id`

func scanTaxJurisdiction(row pgx.Row) (*domain.TaxJurisdiction, error) {
	var j domain.TaxJurisdiction
	if err := row.Scan(&j.BillingAccountID, &j.JurisdictionCode, &j.RegisteredRateBasisPoints,
		&j.EffectiveFrom, &j.CreatedAt, &j.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	return &j, nil
}

// loadTaxJurisdiction mirrors loadCurrency exactly: no row means the
// jurisdiction was never registered by the seller, refused rather than
// taken on faith from the caller (D2).
func loadTaxJurisdiction(ctx context.Context, tx pgx.Tx, billingAccountID, jurisdictionCode string) (*domain.TaxJurisdiction, error) {
	j, err := scanTaxJurisdiction(tx.QueryRow(ctx, `SELECT `+taxJurisdictionColumns+`
		FROM billing_tax_jurisdictions WHERE billing_account_id = $1 AND jurisdiction_code = $2`,
		billingAccountID, jurisdictionCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTaxJurisdictionNotRegistered
	}
	return j, err
}

// RegisterTaxJurisdiction upserts one (billing account, jurisdiction) fact —
// mutable in place, the same idiom as UpsertCurrency, since this is
// seller-managed reference data, not versioned/immutable evidence.
func (s *PgStore) RegisterTaxJurisdiction(ctx context.Context, organizationID string, j *domain.TaxJurisdiction) (*domain.TaxJurisdiction, error) {
	var out *domain.TaxJurisdiction
	err := s.billingSellerTx(ctx, organizationID, func(tx pgx.Tx) error {
		got, err := scanTaxJurisdiction(tx.QueryRow(ctx, `
			INSERT INTO billing_tax_jurisdictions (billing_account_id, jurisdiction_code, organization_id,
				registered_rate_basis_points, effective_from, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (billing_account_id, jurisdiction_code) DO UPDATE
				SET registered_rate_basis_points = EXCLUDED.registered_rate_basis_points,
				    effective_from = EXCLUDED.effective_from
			RETURNING `+taxJurisdictionColumns,
			j.BillingAccountID, j.JurisdictionCode, organizationID, j.RegisteredRateBasisPoints,
			j.EffectiveFrom, j.CreatedAt, j.CreatedByPrincipalID))
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	return out, err
}

// ── D3: shared accounting-event emission ────────────────────────────────────

// emitAccountingEvent records one accounting_event.recorded outbox event,
// classified only by the seller's own server-resolved mapping key
// (BillingAccount.AccountingMappingKey) — never a caller-suppliable field on
// any of the commands that call this, so a tenant-supplied accounting-book
// reference has no field to be injected through (COM-CTRL-033, negative
// path #44 refused by construction).
func emitAccountingEvent(ctx context.Context, tx pgx.Tx, organizationID, mappingKey, amount, direction, sourceType, sourceID string) error {
	return outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "accounting_event", AggregateID: sourceID,
		EventType: "accounting_event.recorded", TenantID: &organizationID,
		Payload: map[string]any{
			"accounting_mapping_key": mappingKey,
			"amount":                 amount,
			"direction":              direction,
			"source_type":            sourceType,
			"source_id":              sourceID,
		},
	})
}

// ── D4: dispute / chargeback ─────────────────────────────────────────────────

// DisputeStore is the COM-05 gap-remediation dispute/chargeback contract.
type DisputeStore interface {
	OpenDispute(ctx context.Context, disputeID, invoiceID, paymentAttemptID, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DisputeCase, error)
	RecordDisputeOutcome(ctx context.Context, disputeID string, outcome domain.DisputeStatus, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DisputeCase, error)
	CloseDispute(ctx context.Context, disputeID, notes, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DisputeCase, error)
	GetDispute(ctx context.Context, disputeID string) (*domain.DisputeCase, error)
}

var _ DisputeStore = (*PgStore)(nil)

var disputeImmutableTables = []string{"dispute_cases", "dispute case"}

func mapDisputeErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code == "CP001" {
		for _, t := range disputeImmutableTables {
			if strings.Contains(pgErr.Message, t) {
				return fmt.Errorf("%w: %s", domain.ErrDisputeCaseInvalidState, pgErr.Message)
			}
		}
	}
	return err
}

func (s *PgStore) disputeSellerTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapDisputeErr(s.withSellerPlane(ctx, fn))
}

func (s *PgStore) disputeTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapDisputeErr(s.withTenant(ctx, fn))
}

const disputeCaseColumns = `dispute_id, organization_id::text, invoice_id, payment_attempt_id, status, reason,
	opened_at, opened_by_principal_id, outcome_recorded_at, outcome_recorded_by_principal_id,
	resolved_at, resolved_by_principal_id, resolution_notes, related_refund_id`

func scanDisputeCase(row pgx.Row) (*domain.DisputeCase, error) {
	var d domain.DisputeCase
	if err := row.Scan(&d.DisputeID, &d.OrganizationID, &d.InvoiceID, &d.PaymentAttemptID, &d.Status, &d.Reason,
		&d.OpenedAt, &d.OpenedByPrincipalID, &d.OutcomeRecordedAt, &d.OutcomeRecordedByPrincipalID,
		&d.ResolvedAt, &d.ResolvedByPrincipalID, &d.ResolutionNotes, &d.RelatedRefundID); err != nil {
		return nil, err
	}
	return &d, nil
}

func loadDisputeCase(ctx context.Context, tx pgx.Tx, disputeID string, forUpdate bool) (*domain.DisputeCase, error) {
	q := `SELECT ` + disputeCaseColumns + ` FROM dispute_cases WHERE dispute_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	d, err := scanDisputeCase(tx.QueryRow(ctx, q, disputeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDisputeCaseNotFound
	}
	return d, err
}

// OpenDispute opens a purely orthogonal tracking record against a SUCCEEDED
// payment attempt — it never writes to payment_attempts or
// platform_commercial_invoices (ZS-SVC-Q-001 §6: "preserve paid collection
// history and separate dispute state").
func (s *PgStore) OpenDispute(ctx context.Context, disputeID, invoiceID, paymentAttemptID, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DisputeCase, error) {
	var out *domain.DisputeCase
	err := s.disputeSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		attempt, err := loadPaymentAttempt(ctx, tx, paymentAttemptID, false)
		if err != nil {
			return err
		}
		if attempt.InvoiceID != invoiceID {
			return domain.ErrPaymentAttemptNotFound
		}
		if attempt.Status != domain.PaymentSucceeded {
			return domain.ErrDisputeAttemptNotSucceeded
		}
		got, err := scanDisputeCase(tx.QueryRow(ctx, `
			INSERT INTO dispute_cases (dispute_id, organization_id, invoice_id, payment_attempt_id, status, reason,
				opened_at, opened_by_principal_id)
			VALUES ($1, $2, $3, $4, 'OPEN', $5, $6, $7) RETURNING `+disputeCaseColumns,
			disputeID, attempt.OrganizationID, invoiceID, paymentAttemptID, reason, now, actor))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dispute_case", AggregateID: disputeID,
			EventType: "dispute.opened", TenantID: &attempt.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) RecordDisputeOutcome(ctx context.Context, disputeID string, outcome domain.DisputeStatus, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DisputeCase, error) {
	var out *domain.DisputeCase
	err := s.disputeSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		d, err := loadDisputeCase(ctx, tx, disputeID, true)
		if err != nil {
			return err
		}
		if d.Status != domain.DisputeOpen {
			return fmt.Errorf("%w: dispute is %s, must be OPEN to record an outcome", domain.ErrDisputeCaseInvalidState, d.Status)
		}
		if outcome != domain.DisputeWon && outcome != domain.DisputeLost {
			return fmt.Errorf("%w: outcome must be WON or LOST", domain.ErrDisputeCaseInvalidState)
		}
		if _, err := tx.Exec(ctx, `UPDATE dispute_cases SET status = $2, outcome_recorded_at = $3,
			outcome_recorded_by_principal_id = $4 WHERE dispute_id = $1`, disputeID, outcome, now, actor); err != nil {
			return err
		}
		got, err := loadDisputeCase(ctx, tx, disputeID, false)
		if err != nil {
			return err
		}
		out = got
		eventType := "dispute.won"
		if outcome == domain.DisputeLost {
			eventType = "dispute.lost"
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dispute_case", AggregateID: disputeID,
			EventType: eventType, TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) CloseDispute(ctx context.Context, disputeID, notes, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.DisputeCase, error) {
	var out *domain.DisputeCase
	err := s.disputeSellerTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		d, err := loadDisputeCase(ctx, tx, disputeID, true)
		if err != nil {
			return err
		}
		if d.Status != domain.DisputeWon && d.Status != domain.DisputeLost {
			return fmt.Errorf("%w: dispute is %s, must be WON or LOST to close", domain.ErrDisputeCaseInvalidState, d.Status)
		}
		var notesArg *string
		if notes != "" {
			notesArg = &notes
		}
		if _, err := tx.Exec(ctx, `UPDATE dispute_cases SET status = 'RESOLVED', resolved_at = $2,
			resolved_by_principal_id = $3, resolution_notes = $4 WHERE dispute_id = $1`,
			disputeID, now, actor, notesArg); err != nil {
			return err
		}
		got, err := loadDisputeCase(ctx, tx, disputeID, false)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dispute_case", AggregateID: disputeID,
			EventType: "dispute.resolved", TenantID: &got.OrganizationID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetDispute(ctx context.Context, disputeID string) (*domain.DisputeCase, error) {
	var out *domain.DisputeCase
	err := s.disputeTx(ctx, func(tx pgx.Tx) error {
		d, err := loadDisputeCase(ctx, tx, disputeID, false)
		out = d
		return err
	})
	return out, err
}
