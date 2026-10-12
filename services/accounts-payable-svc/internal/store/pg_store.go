// Package store provides the PostgreSQL implementation of accounts-payable-svc's
// persistence layer (AP-05 supplier invoices and the AP-06 matching module).
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction -- the Row-Level Security policies in deployments/migrations are
// real. But every method ALSO filters explicitly by tenant_id in its own SQL,
// rather than relying on RLS alone: this pool connects as a Postgres superuser
// (DB_USER=postgres, same as every other service in this platform), and
// superusers unconditionally bypass Row-Level Security regardless of policy.
//
// State changes go through one primitive, mutateTx: lock the invoice row,
// check expected_version, let the command mutate the invoice in memory, then
// write the invoice, the append-only history row, the outbox events and any
// side records (duplicate assessment, correction link, AP-08 payable-creation
// queue row) in the SAME transaction.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
	"zoiko.io/accounts-payable-svc/internal/outbox"
)

// identityColumns are written once at creation and never updated.
var identityColumns = []string{
	"invoice_id", "tenant_id", "legal_entity_id", "vendor_id", "invoice_number",
	"currency_code", "source_contract_id", "created_by_principal_id", "correlation_id",
	"created_at", "document_type", "invoice_number_normalized", "source_channel",
}

// mutableColumns are rewritten by every command (the same UPDATE for all of
// them, so no command can forget a column). The immutability trigger -- not this
// list -- is what stops changes to source columns after acceptance.
var mutableColumns = []string{
	"amount", "due_date", "invoice_date", "supply_date", "net_amount", "tax_amount",
	"purchase_order_id", "goods_receipt_ref", "po_vendor_profile_id", "invoice_document_id",
	"status",
	"validated_by_principal_id", "approved_by_principal_id", "payment_requested_by_principal_id",
	"validated_at", "approved_at", "payment_requested_at",
	"intake_state", "match_state", "approval_state", "accounting_state", "settlement_state",
	"hold_state", "hold_reason",
	"source_hash", "attachment_hash", "source_accepted_at",
	"duplicate_state", "quarantine_reason",
	"submitted_by_principal_id", "submitted_at", "rejected_by_principal_id", "rejected_at", "reject_reason",
	"tax_state", "tax_provenance", "tax_result_hash", "tax_verified_at",
	"withholding_ref", "withholding_determination_id", "withholding_provenance",
	"extracted_bank_details", "payee_state", "payee_check",
	"supplier_profile_id", "supplier_profile_version", "po_revision",
	"match_required", "match_cleared", "match_run_id",
	"payable_id", "accounting_event_id", "approval_journal_id",
}

// invoiceColumns is the single source of truth for the read shape. Every SELECT
// derives its column list from it and scans through scanRefs in the same order;
// init() refuses to start if the two diverge.
var invoiceColumns = append(append(append([]string{}, identityColumns...), mutableColumns...), "version")

var invoiceSelectList = strings.Join(invoiceColumns, ", ")

// raw holds the columns that need conversion after the scan.
type raw struct {
	status, intake, match, approval, accounting, settlement, hold, dup, tax, payee string
	taxProv, withholding, bank, payeeCheck                                         []byte
	docType                                                                        string
}

func scanRefs(inv *domain.VendorInvoice, r *raw) []any {
	return []any{
		// identity
		&inv.InvoiceID, &inv.TenantID, &inv.LegalEntityID, &inv.VendorID, &inv.InvoiceNumber,
		&inv.CurrencyCode, &inv.SourceContractID, &inv.CreatedByPrincipalID, &inv.CorrelationID,
		&inv.CreatedAt, &r.docType, &inv.InvoiceNumberNormalized, &inv.SourceChannel,
		// mutable
		&inv.Amount, &inv.DueDate, &inv.InvoiceDate, &inv.SupplyDate, &inv.NetAmount, &inv.TaxAmount,
		&inv.PurchaseOrderID, &inv.GoodsReceiptRef, &inv.POVendorProfileID, &inv.InvoiceDocumentID,
		&r.status,
		&inv.ValidatedByPrincipalID, &inv.ApprovedByPrincipalID, &inv.PaymentRequestedByPrincipalID,
		&inv.ValidatedAt, &inv.ApprovedAt, &inv.PaymentRequestedAt,
		&r.intake, &r.match, &r.approval, &r.accounting, &r.settlement,
		&r.hold, &inv.HoldReason,
		&inv.SourceHash, &inv.AttachmentHash, &inv.SourceAcceptedAt,
		&r.dup, &inv.QuarantineReason,
		&inv.SubmittedByPrincipalID, &inv.SubmittedAt, &inv.RejectedByPrincipalID, &inv.RejectedAt, &inv.RejectReason,
		&r.tax, &r.taxProv, &inv.TaxResultHash, &inv.TaxVerifiedAt,
		&inv.WithholdingRef, &inv.WithholdingDeterminationID, &r.withholding,
		&r.bank, &r.payee, &r.payeeCheck,
		&inv.SupplierProfileID, &inv.SupplierProfileVersion, &inv.PORevision,
		&inv.MatchRequired, &inv.MatchCleared, &inv.MatchRunID,
		&inv.PayableID, &inv.AccountingEventID, &inv.ApprovalJournalID,
		// version
		&inv.Version,
	}
}

// finish converts the raw columns into the typed invoice fields.
func (r *raw) finish(inv *domain.VendorInvoice) error {
	inv.DocumentType = r.docType
	inv.Status = domain.InvoiceStatus(r.status)
	inv.IntakeState = domain.IntakeState(r.intake)
	inv.MatchState = domain.MatchState(r.match)
	inv.ApprovalState = domain.ApprovalState(r.approval)
	inv.AccountingState = domain.AccountingState(r.accounting)
	inv.SettlementState = domain.SettlementState(r.settlement)
	inv.HoldState = domain.HoldState(r.hold)
	inv.DuplicateState = domain.DuplicateState(r.dup)
	inv.TaxState = domain.TaxState(r.tax)
	inv.PayeeState = domain.PayeeState(r.payee)
	inv.TaxProvenance, inv.WithholdingProvenance, inv.ExtractedBankDetails, inv.PayeeCheck = nil, nil, nil, nil
	if len(r.taxProv) > 0 {
		if err := json.Unmarshal(r.taxProv, &inv.TaxProvenance); err != nil {
			return fmt.Errorf("decode tax_provenance: %w", err)
		}
	}
	if len(r.withholding) > 0 {
		inv.WithholdingProvenance = new(domain.TaxProvenance)
		if err := json.Unmarshal(r.withholding, inv.WithholdingProvenance); err != nil {
			return fmt.Errorf("decode withholding_provenance: %w", err)
		}
	}
	if len(r.bank) > 0 {
		inv.ExtractedBankDetails = new(domain.BankDetails)
		if err := json.Unmarshal(r.bank, inv.ExtractedBankDetails); err != nil {
			return fmt.Errorf("decode extracted_bank_details: %w", err)
		}
	}
	if len(r.payeeCheck) > 0 {
		inv.PayeeCheck = new(domain.PayeeCheck)
		if err := json.Unmarshal(r.payeeCheck, inv.PayeeCheck); err != nil {
			return fmt.Errorf("decode payee_check: %w", err)
		}
	}
	return nil
}

func jsonOrNil(v any, isNil bool) any {
	if isNil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

// identityValues / mutableValues mirror identityColumns / mutableColumns.
func identityValues(inv *domain.VendorInvoice) []any {
	return []any{
		inv.InvoiceID, inv.TenantID, inv.LegalEntityID, inv.VendorID, inv.InvoiceNumber,
		inv.CurrencyCode, inv.SourceContractID, inv.CreatedByPrincipalID, inv.CorrelationID,
		inv.CreatedAt, inv.DocumentType, inv.InvoiceNumberNormalized, inv.SourceChannel,
	}
}

func mutableValues(inv *domain.VendorInvoice) []any {
	return []any{
		inv.Amount, inv.DueDate, inv.InvoiceDate, inv.SupplyDate, inv.NetAmount, inv.TaxAmount,
		inv.PurchaseOrderID, inv.GoodsReceiptRef, inv.POVendorProfileID, inv.InvoiceDocumentID,
		string(inv.Status),
		inv.ValidatedByPrincipalID, inv.ApprovedByPrincipalID, inv.PaymentRequestedByPrincipalID,
		inv.ValidatedAt, inv.ApprovedAt, inv.PaymentRequestedAt,
		string(inv.IntakeState), string(inv.MatchState), string(inv.ApprovalState), string(inv.AccountingState),
		string(inv.SettlementState), string(inv.HoldState), inv.HoldReason,
		inv.SourceHash, inv.AttachmentHash, inv.SourceAcceptedAt,
		string(inv.DuplicateState), inv.QuarantineReason,
		inv.SubmittedByPrincipalID, inv.SubmittedAt, inv.RejectedByPrincipalID, inv.RejectedAt, inv.RejectReason,
		string(inv.TaxState), jsonOrNil(inv.TaxProvenance, len(inv.TaxProvenance) == 0), inv.TaxResultHash, inv.TaxVerifiedAt,
		inv.WithholdingRef, inv.WithholdingDeterminationID, jsonOrNil(inv.WithholdingProvenance, inv.WithholdingProvenance == nil),
		jsonOrNil(inv.ExtractedBankDetails, inv.ExtractedBankDetails == nil), string(inv.PayeeState), jsonOrNil(inv.PayeeCheck, inv.PayeeCheck == nil),
		inv.SupplierProfileID, inv.SupplierProfileVersion, inv.PORevision,
		inv.MatchRequired, inv.MatchCleared, inv.MatchRunID,
		inv.PayableID, inv.AccountingEventID, inv.ApprovalJournalID,
	}
}

// lineColumns and lineScanTargets are the same pair for vendor_invoice_lines.
var lineColumns = []string{
	"invoice_line_id", "invoice_id", "line_number", "description", "quantity", "unit_price",
	"net_amount", "tax_code", "tax_amount", "tax_determination_id", "po_line_reference", "dimensions",
}

var lineSelectList = strings.Join(lineColumns, ", ")

func lineScanTargets(l *domain.VendorInvoiceLine) []any {
	return []any{
		&l.InvoiceLineID, &l.InvoiceID, &l.LineNumber, &l.Description, &l.Quantity, &l.UnitPrice,
		&l.NetAmount, &l.TaxCode, &l.TaxAmount, &l.TaxDeterminationID, &l.POLineReference, &l.Dimensions,
	}
}

func init() {
	// A count mismatch is a guaranteed runtime scan error on the first read, so
	// failing at startup is strictly better than failing per-request later.
	if n, c := len(scanRefs(&domain.VendorInvoice{}, &raw{})), len(invoiceColumns); n != c {
		panic(fmt.Sprintf("store: %d scan targets for %d invoice columns", n, c))
	}
	if n, c := len(identityValues(&domain.VendorInvoice{})), len(identityColumns); n != c {
		panic(fmt.Sprintf("store: %d identity values for %d columns", n, c))
	}
	if n, c := len(mutableValues(&domain.VendorInvoice{})), len(mutableColumns); n != c {
		panic(fmt.Sprintf("store: %d mutable values for %d columns", n, c))
	}
	if n, c := len(lineScanTargets(&domain.VendorInvoiceLine{})), len(lineColumns); n != c {
		panic(fmt.Sprintf("store: %d scan targets for %d invoice line columns", n, c))
	}
}

// Constraint names from the migrations.
const (
	constraintVendorInvoiceNumber = "vendor_invoices_tenant_id_vendor_id_invoice_number_key"
)

// mapPgError translates the Postgres failures that are really caller mistakes
// into domain errors, so they stop arriving as "the store is unavailable".
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "22P02":
		// invalid_text_representation -- a non-UUID compared against a uuid column.
		return domain.ErrInvalidIdentifier
	case "23505":
		if pgErr.ConstraintName == constraintVendorInvoiceNumber {
			return domain.ErrDuplicateInvoiceNumber
		}
		return err
	case "23000":
		// The AP-06 match gate is a state-machine refusal, not an immutability one.
		if strings.Contains(pgErr.Message, "AP-06 match") {
			return fmt.Errorf("%w: %s", domain.ErrInvalidTransition, pgErr.Message)
		}
		// Raised by the immutability triggers.
		return fmt.Errorf("%w: %s", domain.ErrInvoiceImmutable, pgErr.Message)
	default:
		return err
	}
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
	keys domain.PostingMappingKeys
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// Pool exposes the pool to the background relays.
func (s *PgStore) Pool() *pgxpool.Pool { return s.pool }

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return s.withRLSOpts(ctx, tenantID, pgx.TxOptions{}, fn)
}

func (s *PgStore) withRLSOpts(ctx context.Context, tenantID string, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── create ───────────────────────────────────────────────────────────────────

// CreateInvoice inserts an invoice (RECEIVED, or QUARANTINED when mut carries a
// near-duplicate assessment) with its lines, history row, assessment and outbox
// events in one transaction.
//
// Idempotent on (tenant_id, correlation_id): a retried call resolves to the
// ORIGINAL invoice -- mutating *inv in place -- and returns created=false.
// A re-keyed invoice number surfaces as domain.ErrDuplicateInvoiceNumber.
func (s *PgStore) CreateInvoice(ctx context.Context, inv *domain.VendorInvoice, mut *domain.Mutation) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrTenantScopeMissing
	}
	inv.TenantID = tenantID
	canonical := applyCreateDefaults(inv)
	if mut == nil {
		// The plain header/lines contract: record the received event and the
		// canonical source payload atomically with the insert.
		mut = &domain.Mutation{
			Command: "CaptureSupplierInvoice", Actor: inv.CreatedByPrincipalID, CorrelationID: inv.CorrelationID,
			NewSourcePayload: canonical,
			Events: []domain.OutboxEvent{{EventType: "vendor.invoice.received", Payload: map[string]any{
				"invoice_id": inv.InvoiceID, "tenant_id": inv.TenantID,
				"legal_entity_id": inv.LegalEntityID, "vendor_id": inv.VendorID,
			}}},
		}
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		inv.CreatedAt = now
		inv.Version = 1

		cols := append(append([]string{}, identityColumns...), mutableColumns...)
		vals := append(identityValues(inv), mutableValues(inv)...)
		cols = append(cols, "version", "source_payload")
		vals = append(vals, inv.Version, []byte(mut.NewSourcePayload))
		ph := make([]string, len(cols))
		for i := range ph {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO vendor_invoices (`+strings.Join(cols, ", ")+`) VALUES (`+strings.Join(ph, ", ")+`)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING`, vals...)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			// Replay of a prior submission: return the ORIGINAL invoice in full.
			existing, err := loadInvoice(ctx, tx, tenantID, "", inv.CorrelationID, false)
			if err != nil {
				return err
			}
			*inv = *existing
			created = false
			return nil
		}

		for i := range inv.Lines {
			inv.Lines[i].InvoiceLineID = uuid.NewString()
			inv.Lines[i].InvoiceID = inv.InvoiceID
			if err := insertLine(ctx, tx, tenantID, &inv.Lines[i]); err != nil {
				return err
			}
		}
		created = true

		if err := s.writeSideRecords(ctx, tx, inv, nil, mut); err != nil {
			return err
		}
		return nil
	})
	return created, err
}

func insertLine(ctx context.Context, tx pgx.Tx, tenantID string, l *domain.VendorInvoiceLine) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO vendor_invoice_lines (
			invoice_line_id, invoice_id, tenant_id, line_number,
			description, quantity, unit_price, net_amount,
			tax_code, tax_amount, tax_determination_id, po_line_reference, dimensions
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		l.InvoiceLineID, l.InvoiceID, tenantID, l.LineNumber,
		l.Description, l.Quantity, l.UnitPrice, l.NetAmount,
		l.TaxCode, l.TaxAmount, l.TaxDeterminationID, l.POLineReference, l.Dimensions)
	return mapPgError(err)
}

// ── read ─────────────────────────────────────────────────────────────────────

// loadInvoice reads one invoice by id (or, when id is empty, by correlation id)
// with its lines, scoped to the tenant.
func loadInvoice(ctx context.Context, tx pgx.Tx, tenantID, invoiceID, correlationID string, forUpdate bool) (*domain.VendorInvoice, error) {
	q := `SELECT ` + invoiceSelectList + ` FROM vendor_invoices WHERE tenant_id = $1 AND `
	args := []any{tenantID}
	if invoiceID != "" {
		q += `invoice_id = $2`
		args = append(args, invoiceID)
	} else {
		q += `correlation_id = $2`
		args = append(args, correlationID)
	}
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var inv domain.VendorInvoice
	var r raw
	if err := tx.QueryRow(ctx, q, args...).Scan(scanRefs(&inv, &r)...); err != nil {
		return nil, mapPgError(err)
	}
	if err := r.finish(&inv); err != nil {
		return nil, err
	}
	lines, err := queryLines(ctx, tx, tenantID, inv.InvoiceID)
	if err != nil {
		return nil, err
	}
	inv.Lines = lines
	return &inv, nil
}

// GetInvoice returns a vendor invoice by ID, scoped to the caller's tenant.
// Returns (nil, nil) if not found -- including another tenant's invoice and a
// malformed id, which must be indistinguishable from outside.
func (s *PgStore) GetInvoice(ctx context.Context, invoiceID string) (*domain.VendorInvoice, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}
	var inv *domain.VendorInvoice
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		inv, err = loadInvoice(ctx, tx, tenantID, invoiceID, "", false)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

func queryLines(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string) ([]domain.VendorInvoiceLine, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+lineSelectList+`
		 FROM vendor_invoice_lines
		 WHERE invoice_id = $1 AND tenant_id = $2
		 ORDER BY line_number ASC`, invoiceID, tenantID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()

	var lines []domain.VendorInvoiceLine
	for rows.Next() {
		var l domain.VendorInvoiceLine
		if err := rows.Scan(lineScanTargets(&l)...); err != nil {
			return nil, mapPgError(err)
		}
		lines = append(lines, l)
	}
	return lines, mapPgError(rows.Err())
}

// ListInvoices returns vendor invoices matching the given filter.
func (s *PgStore) ListInvoices(ctx context.Context, filter domain.ListInvoicesFilter) ([]domain.VendorInvoice, error) {
	var out []domain.VendorInvoice
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + invoiceSelectList + `
			FROM vendor_invoices
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR vendor_id = $3)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC`
		args := []any{filter.TenantID, filter.LegalEntityID, filter.VendorID, filter.Status}
		if filter.Limit > 0 {
			query += fmt.Sprintf(" LIMIT $%d", len(args)+1)
			args = append(args, filter.Limit)
			if filter.Offset > 0 {
				query += fmt.Sprintf(" OFFSET $%d", len(args)+1)
				args = append(args, filter.Offset)
			}
		}
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var inv domain.VendorInvoice
			var r raw
			if err := rows.Scan(scanRefs(&inv, &r)...); err != nil {
				return err
			}
			if err := r.finish(&inv); err != nil {
				return err
			}
			out = append(out, inv)
		}
		return mapPgError(rows.Err())
	})
	return out, err
}

// ── mutate ───────────────────────────────────────────────────────────────────

// MutateInvoice is the one primitive every AP-05 state change goes through.
func (s *PgStore) MutateInvoice(ctx context.Context, tenantID, invoiceID string, expectedVersion *int, fn domain.MutateFn) (*domain.VendorInvoice, error) {
	var out *domain.VendorInvoice
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		inv, err := s.MutateInTx(ctx, tx, tenantID, invoiceID, expectedVersion, fn)
		out = inv
		return err
	})
	if errors.Is(err, domain.ErrInvalidIdentifier) || errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInvoiceNotFound
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MutateInTx is MutateInvoice's body, callable from another store method that
// already owns a transaction (the AP-06 match store updates the invoice's match
// dimension atomically with the run it saves).
func (s *PgStore) MutateInTx(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string, expectedVersion *int, fn domain.MutateFn) (*domain.VendorInvoice, error) {
	inv, err := loadInvoice(ctx, tx, tenantID, invoiceID, "", true)
	if err != nil {
		return nil, err
	}
	if expectedVersion != nil && *expectedVersion != inv.Version {
		return nil, domain.ErrStaleVersion
	}
	before, _ := json.Marshal(inv.StateDimensions())
	oldVersion := inv.Version

	mut, err := fn(inv)
	if err != nil {
		return nil, err
	}
	if mut == nil {
		return inv, nil
	}
	inv.Status = domain.DeriveStatus(inv)
	if !mut.KeepVersion {
		inv.Version = oldVersion + 1
	}

	set := make([]string, 0, len(mutableColumns)+2)
	vals := append([]any{}, mutableValues(inv)...)
	for i, c := range mutableColumns {
		set = append(set, fmt.Sprintf("%s = $%d", c, i+1))
	}
	n := len(vals)
	set = append(set, fmt.Sprintf("version = $%d", n+1))
	vals = append(vals, inv.Version)
	n++
	if mut.NewSourcePayload != nil {
		set = append(set, fmt.Sprintf("source_payload = $%d", n+1))
		vals = append(vals, []byte(mut.NewSourcePayload))
		n++
	}
	vals = append(vals, invoiceID, tenantID, oldVersion)
	tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE vendor_invoices SET %s WHERE invoice_id = $%d AND tenant_id = $%d AND version = $%d`,
		strings.Join(set, ", "), n+1, n+2, n+3), vals...)
	if err != nil {
		return nil, mapPgError(err)
	}
	if tag.RowsAffected() != 1 {
		return nil, domain.ErrStaleVersion
	}

	if mut.ReplaceLines != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM vendor_invoice_lines WHERE invoice_id = $1 AND tenant_id = $2`, invoiceID, tenantID); err != nil {
			return nil, mapPgError(err)
		}
		inv.Lines = *mut.ReplaceLines
		for i := range inv.Lines {
			inv.Lines[i].InvoiceLineID = uuid.NewString()
			inv.Lines[i].InvoiceID = invoiceID
			inv.Lines[i].LineNumber = i + 1
			if err := insertLine(ctx, tx, tenantID, &inv.Lines[i]); err != nil {
				return nil, err
			}
		}
	}
	if err := s.writeSideRecords(ctx, tx, inv, before, mut); err != nil {
		return nil, err
	}
	return inv, nil
}

// writeSideRecords writes, in the caller's transaction, everything that must
// commit atomically with an invoice change: history, duplicate assessment,
// correction link, the AP-08 payable-creation queue row and the outbox events.
func (s *PgStore) writeSideRecords(ctx context.Context, tx pgx.Tx, inv *domain.VendorInvoice, before []byte, mut *domain.Mutation) error {
	after, _ := json.Marshal(inv.StateDimensions())
	detail, _ := json.Marshal(mut.Detail)
	if mut.Detail == nil {
		detail = nil
	}
	if err := insertHistory(ctx, tx, inv.TenantID, domain.HistoryEntry{
		InvoiceID: inv.InvoiceID, Version: inv.Version, Command: mut.Command, ActorID: mut.Actor,
		Reason: mut.Reason, FromState: before, ToState: after, Detail: detail, CorrelationID: mut.CorrelationID,
	}); err != nil {
		return err
	}

	if a := mut.Assessment; a != nil {
		a.AssessmentID = uuid.NewString()
		a.TenantID, a.InvoiceID = inv.TenantID, inv.InvoiceID
		a.TriggerCommand = mut.Command
		inputs, _ := json.Marshal(a.Inputs)
		cfg, _ := json.Marshal(a.Config)
		ids, _ := json.Marshal(a.MatchedInvoiceIDs)
		matches, _ := json.Marshal(a.Matches)
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_duplicate_assessments (assessment_id, tenant_id, invoice_id, verdict, score,
				exact_key, near_key, inputs, config, matched_invoice_ids, matches, trigger_command)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			a.AssessmentID, a.TenantID, a.InvoiceID, a.Verdict, a.Score, a.ExactKey, a.NearKey,
			inputs, cfg, ids, matches, a.TriggerCommand); err != nil {
			return mapPgError(err)
		}
	}

	if l := mut.CorrectionLink; l != nil {
		if l.LinkID == "" {
			l.LinkID = uuid.NewString()
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice_correction_links (link_id, tenant_id, original_invoice_id, linked_invoice_id, link_kind, reason, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			l.LinkID, inv.TenantID, l.OriginalInvoiceID, l.LinkedInvoiceID, l.LinkKind, l.Reason, l.CreatedBy); err != nil {
			return mapPgError(err)
		}
		other := l.LinkedInvoiceID
		if inv.InvoiceID == l.LinkedInvoiceID {
			other = l.OriginalInvoiceID
		}
		// The linked document's own history records the link; its row is NOT touched.
		d, _ := json.Marshal(map[string]any{"link_id": l.LinkID, "link_kind": l.LinkKind, "original": l.OriginalInvoiceID, "linked": l.LinkedInvoiceID})
		var v int
		if err := tx.QueryRow(ctx, `SELECT version FROM vendor_invoices WHERE invoice_id = $1 AND tenant_id = $2`, other, inv.TenantID).Scan(&v); err != nil {
			return mapPgError(err)
		}
		if err := insertHistory(ctx, tx, inv.TenantID, domain.HistoryEntry{
			InvoiceID: other, Version: v, Command: "CorrectionLinked", ActorID: mut.Actor, Reason: l.Reason, Detail: d, CorrelationID: mut.CorrelationID,
		}); err != nil {
			return err
		}
	}

	if p := mut.PayableCreation; p != nil {
		payload, _ := json.Marshal(p)
		corr := mut.CorrelationID
		if corr == "" {
			corr = inv.CorrelationID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payable_creation_requests (request_id, tenant_id, legal_entity_id, invoice_id, source_reference, payload, principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (tenant_id, invoice_id) DO NOTHING`,
			uuid.NewString(), inv.TenantID, inv.LegalEntityID, inv.InvoiceID, p.SourceReference, payload, mut.PayablePrincipal, corr); err != nil {
			return mapPgError(err)
		}
	}

	if p := mut.AccountingPosting; p != nil {
		payload, _ := json.Marshal(p)
		corr := firstNonEmpty(mut.CorrelationID, inv.CorrelationID)
		if _, err := tx.Exec(ctx, `
			INSERT INTO accounting_posting_requests (request_id, tenant_id, legal_entity_id, invoice_id, source_event_id, request_payload, principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (tenant_id, source_event_id) DO NOTHING`,
			uuid.NewString(), inv.TenantID, inv.LegalEntityID, inv.InvoiceID, p.SourceEventID, payload, mut.Actor, corr); err != nil {
			return mapPgError(err)
		}
	}

	return EnqueueEvents(
ctx, tx, inv.TenantID, inv.LegalEntityID, inv.InvoiceID, "VENDOR_INVOICE", mut.Actor, firstNonEmpty(mut.CorrelationID, inv.CorrelationID), mut.Events)
}

// EnqueueEvents writes the events to the outbox inside the caller's transaction.
func EnqueueEvents(ctx context.Context, tx pgx.Tx, tenantID, legalEntityID, aggregateID, aggregateType, actor, correlationID string, evs []domain.OutboxEvent) error {
	for _, ev := range evs {
		env, err := outbox.NewVariantAEnvelope(ev.EventType, correlationID, tenantID, legalEntityID, actor, ev.Payload)
		if err != nil {
			return fmt.Errorf("build outbox envelope: %w", err)
		}
		a := actor
		if err := outbox.Insert(ctx, tx, outbox.Event{
			AggregateType: aggregateType, AggregateID: aggregateID, EventType: ev.EventType,
			TenantID: tenantID, LegalEntityID: legalEntityID, ActorID: &a, CorrelationID: correlationID, Payload: env,
		}); err != nil {
			return fmt.Errorf("outbox insert: %w", err)
		}
	}
	return nil
}

func insertHistory(ctx context.Context, tx pgx.Tx, tenantID string, h domain.HistoryEntry) error {
	nz := func(b []byte) any {
		if len(b) == 0 {
			return nil
		}
		return []byte(b)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO invoice_history (tenant_id, invoice_id, version, command, actor_id, reason, from_state, to_state, detail, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		tenantID, h.InvoiceID, h.Version, h.Command, h.ActorID, nullable(h.Reason), nz(h.FromState), nz(h.ToState), nz(h.Detail), nullable(h.CorrelationID))
	return mapPgError(err)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// SetApprovalJournalID records the GL journal ID returned when the invoice
// approval accounting event was posted (ACC-14).
func (s *PgStore) SetApprovalJournalID(ctx context.Context, tenantID, invoiceID, journalID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE vendor_invoices SET approval_journal_id = $1 WHERE invoice_id = $2 AND tenant_id = $3`,
			journalID, invoiceID, tenantID)
		return err
	})
}
