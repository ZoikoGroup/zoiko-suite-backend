// BNK-09 Treasury Transfer persistence — see migration
// 000004_add_bnk09_treasury_transfer for the schema this operates on.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/treasury-svc/internal/domain"
)

// protectedHashV2Prefix marks a hash computed from exact decimal text. It
// fits inside the existing VARCHAR(64) protected_field_hash column, so the
// compatibility split needs no schema migration: pre-A34 rows are 64
// lowercase hex characters, new rows are "v2:" plus a 43-character base64url
// SHA-256. Hex cannot contain 'v' or ':', so the two formats are unambiguous.
const protectedHashV2Prefix = "v2:"

const treasuryTransferColumns = `
	transfer_id, tenant_id, source_bank_account_id, target_bank_account_id, amount::text, currency_code, correlation_id, is_cross_entity,
	protected_field_hash, status, maker_principal_id, checker_principal_id, authorizer_principal_id, reject_reason, payment_attempt_id, source_journal_id, intercompany_entry_id,
	cancel_reason, return_reason, resolution_note,
	created_at, updated_at`

func scanTreasuryTransfer(row pgx.Row, t *domain.TreasuryTransfer) error {
	return row.Scan(&t.TransferID, &t.TenantID, &t.SourceBankAccountID, &t.TargetBankAccountID, &t.Amount, &t.CurrencyCode, &t.CorrelationID, &t.IsCrossEntity,
		&t.ProtectedFieldHash, &t.Status, &t.MakerPrincipalID, &t.CheckerPrincipalID, &t.AuthorizerPrincipalID, &t.RejectReason, &t.PaymentAttemptID, &t.SourceJournalID, &t.IntercompanyEntryID,
		&t.CancelReason, &t.ReturnReason, &t.ResolutionNote,
		&t.CreatedAt, &t.UpdatedAt)
}

// protectedFieldHash hashes the fields a treasury transfer's approval must
// stay honest about — recomputed at ApproveTreasuryTransfer time and
// compared against the value captured at creation.
//
// New rows hash the exact Decimal at the column's fixed four-place scale,
// never through float64, and prefix the digest with protectedHashV2Prefix.
// Older rows were hashed with fmt.Sprintf("%.4f", float64) and stored as
// bare hex. Those are not byte-identical to exact decimal formatting for
// every NUMERIC(20,4) value: above roughly 2^39 units the nearest float64 can
// be far enough from the stored decimal that %.4f crosses a 0.0001 boundary
// (for example 596267352650.6170 formats through float64 as
// "596267352650.6169"). The version marker lets
// protectedFieldHashMatches accept the legacy rendering only for old rows,
// without also accepting a legacy rendering of a new row's exact hash. The
// float64 parse is confined to that old-row comparison; it is never used to
// compute a new hash or money.
func protectedFieldHash(amount domain.Decimal, currencyCode, srcAcctID, tgtAcctID string) string {
	sum := protectedFieldDigest(amount.MoneyText(), currencyCode, srcAcctID, tgtAcctID)
	return protectedHashV2Prefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

// legacyProtectedFieldHash reproduces the pre-A34 hash for an amount read
// back from NUMERIC(20,4). It is verification-only compatibility, not a
// money calculation: the exact decimal is parsed to the same float64 the old
// code scanned and formatted with the same %.4f verb. An unparseable value
// returns "", which can never equal a stored digest.
func legacyProtectedFieldHash(amount domain.Decimal, currencyCode, srcAcctID, tgtAcctID string) string {
	f, err := strconv.ParseFloat(amount.MoneyText(), 64)
	if err != nil {
		return ""
	}
	return hashProtectedFields(fmt.Sprintf("%.4f", f), currencyCode, srcAcctID, tgtAcctID)
}

// hashProtectedFields is the pre-A34 64-character hex rendering, retained so
// an exact-hash row written by an intermediate build (before the version
// marker) and a legacy float-formatted row can both still verify.
func hashProtectedFields(amountText, currencyCode, srcAcctID, tgtAcctID string) string {
	sum := protectedFieldDigest(amountText, currencyCode, srcAcctID, tgtAcctID)
	return hex.EncodeToString(sum[:])
}

func protectedFieldDigest(amountText, currencyCode, srcAcctID, tgtAcctID string) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s", amountText, currencyCode, srcAcctID, tgtAcctID)))
}

// protectedFieldHashMatches accepts the exact versioned hash written by this
// pass and, for rows written before it, either the exact unversioned hash or
// the legacy float-formatted hash. A v2 hash is never checked against the
// legacy rendering: at large magnitudes float64 can map two adjacent
// four-decimal amounts to the same legacy text, and a new row must not accept
// that ambiguity. A row is invalid only when no format valid for its stored
// version matches, so tampering still fails closed.
func protectedFieldHashMatches(amount domain.Decimal, currencyCode, srcAcctID, tgtAcctID, stored string) bool {
	if stored == "" {
		return false
	}
	if strings.HasPrefix(stored, protectedHashV2Prefix) {
		return protectedFieldHash(amount, currencyCode, srcAcctID, tgtAcctID) == stored
	}
	return hashProtectedFields(amount.MoneyText(), currencyCode, srcAcctID, tgtAcctID) == stored ||
		legacyProtectedFieldHash(amount, currencyCode, srcAcctID, tgtAcctID) == stored
}

// CreateTreasuryTransfer is BNK-09's maker entry point. Idempotent on
// (tenant_id, correlation_id): a retried create returns the original
// transfer rather than creating a second one.
func (s *PgStore) CreateTreasuryTransfer(ctx context.Context, p domain.CreateTreasuryTransferParams) (*domain.TreasuryTransfer, bool, error) {
	created := false
	var t domain.TreasuryTransfer
	hash := protectedFieldHash(p.Amount, p.CurrencyCode, p.SourceBankAccountID, p.TargetBankAccountID)
	initialStatus := domain.TransferPendingApproval
	if p.SaveAsDraft {
		initialStatus = domain.TransferDraft
	}
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO treasury_transfers (
				tenant_id, source_bank_account_id, target_bank_account_id, amount, currency_code, correlation_id, is_cross_entity,
				protected_field_hash, status, maker_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id <> '' DO NOTHING
			RETURNING `+treasuryTransferColumns,
			p.TenantID, p.SourceBankAccountID, p.TargetBankAccountID, p.Amount, p.CurrencyCode, p.CorrelationID, p.IsCrossEntity,
			hash, initialStatus, p.MakerPrincipalID)
		err := scanTreasuryTransfer(row, &t)
		if err == nil {
			created = true
			// Enqueue treasury_transfer.created event
			return s.enqueueTreasuryTransferEvent(ctx, tx, "created", "treasury_transfer.created", p.TenantID, "", t.TransferID, p.MakerPrincipalID, p.CorrelationID, map[string]any{
				"transfer_id":       t.TransferID,
				"tenant_id":         t.TenantID,
				"source_account_id": t.SourceBankAccountID,
				"target_account_id": t.TargetBankAccountID,
				"amount":            t.Amount,
				"currency_code":     t.CurrencyCode,
				"is_cross_entity":   t.IsCrossEntity,
				"status":            t.Status,
			})
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return scanTreasuryTransfer(tx.QueryRow(ctx, `SELECT `+treasuryTransferColumns+` FROM treasury_transfers WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID), &t)
	})
	if err != nil {
		return nil, false, err
	}
	return &t, created, nil
}

func (s *PgStore) GetTreasuryTransfer(ctx context.Context, tenantID, transferID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTreasuryTransfer(tx.QueryRow(ctx, `SELECT `+treasuryTransferColumns+` FROM treasury_transfers WHERE transfer_id=$1 AND tenant_id=$2`, transferID, tenantID), &t)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTransferNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (s *PgStore) notFoundOrInvalidTransfer(ctx context.Context, tenantID, transferID string) error {
	if _, err := s.GetTreasuryTransfer(ctx, tenantID, transferID); err != nil {
		return err
	}
	return domain.ErrInvalidTransferTransition
}

// ApproveTreasuryTransfer is BNK-09's checker entry point. Enforces two
// real controls before the CAS update ever runs: the checker must differ
// from the maker (also backed by migration 000004's own trigger), and the
// transfer's protected fields must still hash to what they were at
// creation — a mismatch means something altered amount/currency/accounts
// between creation and approval, which must invalidate the approval
// rather than silently proceed.
func (s *PgStore) ApproveTreasuryTransfer(ctx context.Context, p domain.ApproveTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	existing, err := s.GetTreasuryTransfer(ctx, p.TenantID, p.TransferID)
	if err != nil {
		return nil, err
	}
	if existing.MakerPrincipalID == p.CheckerPrincipalID {
		return nil, domain.ErrTransferSelfApproval
	}
	if !protectedFieldHashMatches(existing.Amount, existing.CurrencyCode, existing.SourceBankAccountID, existing.TargetBankAccountID, existing.ProtectedFieldHash) {
		return nil, domain.ErrTransferHashMismatch
	}

	var t domain.TreasuryTransfer
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, checker_principal_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='PENDING_APPROVAL'
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, domain.TransferApproved, p.CheckerPrincipalID)
		if err := scanTreasuryTransfer(row, &t); err != nil {
			return err
		}
		// Enqueue treasury_transfer.approved event
		return s.enqueueTreasuryTransferEvent(ctx, tx, "approved", "treasury_transfer.approved", p.TenantID, "", t.TransferID, p.CheckerPrincipalID, "", map[string]any{
			"transfer_id":       t.TransferID,
			"tenant_id":         t.TenantID,
			"status":            t.Status,
			"checker_principal": t.CheckerPrincipalID,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// AuthorizeTreasuryTransfer is the doc's own additional dual-control step
// for cross-entity transfers — mirrors ApproveTreasuryTransfer's shape
// exactly (self-check before the CAS update, DB trigger as the second
// line of defense). Only valid from APPROVED on a cross-entity transfer;
// same-entity transfers have no AUTHORIZED state to reach at all.
func (s *PgStore) AuthorizeTreasuryTransfer(ctx context.Context, p domain.AuthorizeTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	existing, err := s.GetTreasuryTransfer(ctx, p.TenantID, p.TransferID)
	if err != nil {
		return nil, err
	}
	if existing.MakerPrincipalID == p.AuthorizerPrincipalID {
		return nil, domain.ErrTransferSelfAuthorization
	}
	if !domain.CanAuthorizeTransfer(existing.Status, existing.IsCrossEntity) {
		return nil, domain.ErrInvalidTransferTransition
	}

	var t domain.TreasuryTransfer
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, authorizer_principal_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='APPROVED' AND is_cross_entity=true
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, domain.TransferAuthorized, p.AuthorizerPrincipalID)
		if err := scanTreasuryTransfer(row, &t); err != nil {
			return err
		}
		// Enqueue treasury_transfer.authorized event
		return s.enqueueTreasuryTransferEvent(ctx, tx, "authorized", "treasury_transfer.authorized", p.TenantID, "", t.TransferID, p.AuthorizerPrincipalID, "", map[string]any{
			"transfer_id":          t.TransferID,
			"tenant_id":            t.TenantID,
			"status":               t.Status,
			"authorizer_principal": t.AuthorizerPrincipalID,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTransfers is the doc's own ListTransfers query — tenant-scoped,
// with optional legal_entity_id/status filters. legal_entity_id is
// resolved via source_bank_account_id's owning account (treasury_transfers
// itself has no legal_entity_id column — the transfer's legal entity is
// always the source account's), matching how every other BNK-09 command
// already derives legal entity from the source account.
func (s *PgStore) ListTransfers(ctx context.Context, p domain.ListTransfersParams) ([]domain.TreasuryTransfer, error) {
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var results []domain.TreasuryTransfer
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + treasuryTransferColumns + ` FROM treasury_transfers t
			WHERE t.tenant_id=$1
				AND ($2='' OR EXISTS (SELECT 1 FROM bank_accounts ba WHERE ba.bank_account_id=t.source_bank_account_id AND ba.legal_entity_id=$2))
				AND ($3='' OR t.status=$3)
			ORDER BY t.created_at DESC
			LIMIT $4 OFFSET $5`
		rows, err := tx.Query(ctx, query, p.TenantID, p.LegalEntityID, p.Status, limit, p.Offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.TreasuryTransfer
			if err := scanTreasuryTransfer(rows, &t); err != nil {
				return err
			}
			results = append(results, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if results == nil {
		results = []domain.TreasuryTransfer{}
	}
	return results, nil
}

func (s *PgStore) RejectTreasuryTransfer(ctx context.Context, p domain.RejectTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	existing, err := s.GetTreasuryTransfer(ctx, p.TenantID, p.TransferID)
	if err != nil {
		return nil, err
	}
	if existing.MakerPrincipalID == p.CheckerPrincipalID {
		return nil, domain.ErrTransferSelfApproval
	}
	var t domain.TreasuryTransfer
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, checker_principal_id=$4, reject_reason=$5, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='PENDING_APPROVAL'
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, domain.TransferRejected, p.CheckerPrincipalID, p.Reason)
		if err := scanTreasuryTransfer(row, &t); err != nil {
			return err
		}
		// Enqueue treasury_transfer.rejected event
		return s.enqueueTreasuryTransferEvent(ctx, tx, "rejected", "treasury_transfer.rejected", p.TenantID, "", t.TransferID, p.CheckerPrincipalID, "", map[string]any{
			"transfer_id":       t.TransferID,
			"tenant_id":         t.TenantID,
			"status":            t.Status,
			"reject_reason":     t.RejectReason,
			"checker_principal": t.CheckerPrincipalID,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkTransferSubmitted, MarkTransferLedgerPosted,
// MarkTransferIntercompanyPaired and MarkTransferCompleted are the saga's
// individual step transitions, each a narrow CAS update driven by
// ExecuteTreasuryTransfer's resumable handler loop — see
// internal/handler/bnk09_handler.go.

// MarkTransferSubmitted records BNK-06 accepting the payment attempt. A
// same-entity transfer is submitted from APPROVED; a cross-entity one only
// from AUTHORIZED, the extra dual-control step ExecuteTreasuryTransfer
// requires first. The predecessor is tied to is_cross_entity here too, so
// the store cannot record a cross-entity submission that skipped
// authorization.
func (s *PgStore) MarkTransferSubmitted(ctx context.Context, tenantID, transferID, paymentAttemptID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, payment_attempt_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2
				AND ((status='APPROVED' AND NOT is_cross_entity) OR (status='AUTHORIZED' AND is_cross_entity))
			RETURNING `+treasuryTransferColumns,
			transferID, tenantID, domain.TransferSubmitted, paymentAttemptID)
		if err := scanTreasuryTransfer(row, &t); err != nil {
			return err
		}
		// Enqueue treasury_transfer.submitted event
		return s.enqueueTreasuryTransferEvent(ctx, tx, "submitted", "treasury_transfer.submitted", tenantID, "", transferID, "", paymentAttemptID, map[string]any{
			"transfer_id":        t.TransferID,
			"tenant_id":          t.TenantID,
			"status":             t.Status,
			"payment_attempt_id": t.PaymentAttemptID,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, tenantID, transferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *PgStore) MarkTransferLedgerPosted(ctx context.Context, tenantID, transferID, sourceJournalID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, source_journal_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='SUBMITTED'
			RETURNING `+treasuryTransferColumns,
			transferID, tenantID, domain.TransferLedgerPosted, sourceJournalID)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, tenantID, transferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *PgStore) MarkTransferIntercompanyPaired(ctx context.Context, tenantID, transferID, intercompanyEntryID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, intercompany_entry_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='LEDGER_POSTED'
			RETURNING `+treasuryTransferColumns,
			transferID, tenantID, domain.TransferIntercompanyPaired, intercompanyEntryID)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, tenantID, transferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// AmendTreasuryTransfer changes the transfer's protected fields while
// still DRAFT and recomputes protected_field_hash from the new values —
// only the maker may do this, enforced by fetch-then-compare (same idiom
// as ApproveTreasuryTransfer's self-approval check).
func (s *PgStore) AmendTreasuryTransfer(ctx context.Context, p domain.AmendTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	existing, err := s.GetTreasuryTransfer(ctx, p.TenantID, p.TransferID)
	if err != nil {
		return nil, err
	}
	if existing.MakerPrincipalID != p.ActorPrincipalID {
		return nil, domain.ErrOnlyMakerMayModifyTransfer
	}
	hash := protectedFieldHash(p.Amount, p.CurrencyCode, p.SourceBankAccountID, p.TargetBankAccountID)
	var t domain.TreasuryTransfer
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers
			SET source_bank_account_id=$3, target_bank_account_id=$4, amount=$5, currency_code=$6, protected_field_hash=$7,
			    is_cross_entity=$8, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='DRAFT'
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, p.SourceBankAccountID, p.TargetBankAccountID, p.Amount, p.CurrencyCode, hash, p.IsCrossEntity)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SubmitTransferForApproval is the maker's own DRAFT->PENDING_APPROVAL
// command — distinct from MarkTransferSubmitted, which records BNK-06
// accepting the payment attempt much later in the lifecycle. Only the
// maker may submit their own draft.
func (s *PgStore) SubmitTransferForApproval(ctx context.Context, p domain.SubmitTransferForApprovalParams) (*domain.TreasuryTransfer, error) {
	existing, err := s.GetTreasuryTransfer(ctx, p.TenantID, p.TransferID)
	if err != nil {
		return nil, err
	}
	if existing.MakerPrincipalID != p.ActorPrincipalID {
		return nil, domain.ErrOnlyMakerMayModifyTransfer
	}
	var t domain.TreasuryTransfer
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='DRAFT'
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, domain.TransferPendingApproval)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// CancelBeforeSubmission moves DRAFT/PENDING_APPROVAL/APPROVED/AUTHORIZED
// straight to the terminal CANCELLED state — the same set
// domain.CanCancelBeforeSubmission names: the bank has not seen any of
// them. Only the maker may cancel their own transfer.
func (s *PgStore) CancelBeforeSubmission(ctx context.Context, p domain.CancelBeforeSubmissionParams) (*domain.TreasuryTransfer, error) {
	existing, err := s.GetTreasuryTransfer(ctx, p.TenantID, p.TransferID)
	if err != nil {
		return nil, err
	}
	if existing.MakerPrincipalID != p.ActorPrincipalID {
		return nil, domain.ErrOnlyMakerMayModifyTransfer
	}
	var t domain.TreasuryTransfer
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, cancel_reason=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status IN ('DRAFT','PENDING_APPROVAL','APPROVED','AUTHORIZED')
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, domain.TransferCancelled, p.Reason)
		if err := scanTreasuryTransfer(row, &t); err != nil {
			return err
		}
		// Enqueue treasury_transfer.cancelled event
		return s.enqueueTreasuryTransferEvent(ctx, tx, "cancelled", "treasury_transfer.cancelled", p.TenantID, "", t.TransferID, p.ActorPrincipalID, "", map[string]any{
			"transfer_id":     t.TransferID,
			"tenant_id":       t.TenantID,
			"status":          t.Status,
			"cancel_reason":   t.CancelReason,
			"actor_principal": t.MakerPrincipalID, // reusing field
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkTransferReturned is an operator action (not maker-restricted) — the
// bank has already seen the transfer (SUBMITTED/LEDGER_POSTED/
// INTERCOMPANY_PAIRED) and returned it unexecuted.
func (s *PgStore) MarkTransferReturned(ctx context.Context, p domain.MarkTransferReturnedParams) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, return_reason=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status IN ('SUBMITTED','LEDGER_POSTED','INTERCOMPANY_PAIRED')
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, domain.TransferReturned, p.Reason)
		if err := scanTreasuryTransfer(row, &t); err != nil {
			return err
		}
		// Enqueue treasury_transfer.returned event
		return s.enqueueTreasuryTransferEvent(ctx, tx, "returned", "treasury_transfer.returned", p.TenantID, "", t.TransferID, p.ActorPrincipalID, "", map[string]any{
			"transfer_id":     t.TransferID,
			"tenant_id":       t.TenantID,
			"status":          t.Status,
			"return_reason":   t.ReturnReason,
			"actor_principal": t.AuthorizerPrincipalID, // reusing field, but should be actor
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ResolveTreasuryTransfer moves a RETURNED transfer onward: RESUBMIT
// clears the failed attempt's correlation ids and the prior checker (a
// resubmission needs a fresh maker-checker cycle, not the old approval)
// and returns it to PENDING_APPROVAL; CANCEL is terminal.
func (s *PgStore) ResolveTreasuryTransfer(ctx context.Context, p domain.ResolveTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	if p.Resolution != domain.ResolutionResubmit && p.Resolution != domain.ResolutionCancel {
		return nil, domain.ErrInvalidResolution
	}
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var row pgx.Row
		if p.Resolution == domain.ResolutionResubmit {
			// A fresh maker-checker cycle: the prior checker, the prior
			// cross-entity authorizer and the failed attempt's correlation
			// ids are cleared, not reused.
			row = tx.QueryRow(ctx, `
				UPDATE treasury_transfers
				SET status=$3, resolution_note=$4, updated_at=now(),
				    checker_principal_id='', authorizer_principal_id='',
				    payment_attempt_id='', source_journal_id='', intercompany_entry_id=''
				WHERE transfer_id=$1 AND tenant_id=$2 AND status='RETURNED'
				RETURNING `+treasuryTransferColumns,
				p.TransferID, p.TenantID, domain.TransferPendingApproval, p.Note)
		} else {
			row = tx.QueryRow(ctx, `
				UPDATE treasury_transfers
				SET status=$3, resolution_note=$4, updated_at=now()
				WHERE transfer_id=$1 AND tenant_id=$2 AND status='RETURNED'
				RETURNING `+treasuryTransferColumns,
				p.TransferID, p.TenantID, domain.TransferCancelled, p.Note)
		}
		if err := scanTreasuryTransfer(row, &t); err != nil {
			return err
		}
		// Enqueue treasury_transfer.cancelled event only for CANCEL resolution
		if p.Resolution == domain.ResolutionCancel {
			return s.enqueueTreasuryTransferEvent(ctx, tx, "cancelled", "treasury_transfer.cancelled", p.TenantID, "", t.TransferID, p.ActorPrincipalID, "", map[string]any{
				"transfer_id":     t.TransferID,
				"tenant_id":       t.TenantID,
				"status":          t.Status,
				"resolution_note": t.ResolutionNote,
				"actor_principal": t.MakerPrincipalID,
			})
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkTransferCompleted accepts either SUBMITTED (the same-entity path,
// which never touches general-ledger-svc/intercompany-accounting-svc at
// all) or INTERCOMPANY_PAIRED (the cross-entity path) as its predecessor
// state — ExecuteTreasuryTransfer picks the right one to call from based
// on IsCrossEntity.
func (s *PgStore) MarkTransferCompleted(ctx context.Context, tenantID, transferID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status IN ('SUBMITTED','INTERCOMPANY_PAIRED')
			RETURNING `+treasuryTransferColumns,
			transferID, tenantID, domain.TransferCompleted)
		if err := scanTreasuryTransfer(row, &t); err != nil {
			return err
		}
		// Enqueue treasury_transfer.settled event
		return s.enqueueTreasuryTransferEvent(ctx, tx, "settled", "treasury_transfer.settled", tenantID, "", transferID, "", "", map[string]any{
			"transfer_id": t.TransferID,
			"tenant_id":   t.TenantID,
			"status":      t.Status,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, tenantID, transferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
