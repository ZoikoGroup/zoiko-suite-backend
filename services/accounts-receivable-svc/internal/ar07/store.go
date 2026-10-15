package ar07

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// querier is satisfied by pgx.Tx and the pool.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const itemCols = `open_item_id::text, tenant_id::text, legal_entity_id::text, document_kind, source_document_id::text, document_number,
	customer_party_id::text, currency, original_amount, residual_amount, to_char(issue_date,'YYYY-MM-DD'), to_char(due_date,'YYYY-MM-DD'),
	business_calendar_id, state, disputed, collection_hold, COALESCE(hold_reason,''), closed_at, version, created_by, created_at, updated_at`

func scanItem(row pgx.Row) (*OpenItem, error) {
	var o OpenItem
	err := row.Scan(&o.OpenItemID, &o.TenantID, &o.LegalEntityID, &o.DocumentKind, &o.SourceDocumentID, &o.DocumentNumber,
		&o.CustomerPartyID, &o.Currency, &o.OriginalAmount, &o.Residual, &o.IssueDate, &o.DueDate,
		&o.BusinessCalendarID, &o.State, &o.Disputed, &o.CollectionHold, &o.HoldReason, &o.ClosedAt, &o.Version, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func notFound() error { return kit.NotFound("open item not found") }

// getItem reads an open item scoped by tenant (and, when non-empty, legal entity). Absent and cross-tenant are identical.
func getItem(ctx context.Context, q querier, tenantID, openItemID string, lock bool) (*OpenItem, error) {
	if err := mustUUID("open_item_id", openItemID); err != nil {
		return nil, err
	}
	sql := `SELECT ` + itemCols + ` FROM ar07_open_items WHERE tenant_id = $1 AND open_item_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	o, err := scanItem(q.QueryRow(ctx, sql, tenantID, openItemID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	return o, err
}

func getItemBySource(ctx context.Context, q querier, tenantID, sourceDocID string) (*OpenItem, error) {
	if err := mustUUID("source_document_id", sourceDocID); err != nil {
		return nil, err
	}
	o, err := scanItem(q.QueryRow(ctx, `SELECT `+itemCols+` FROM ar07_open_items WHERE tenant_id = $1 AND source_document_id = $2`, tenantID, sourceDocID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	return o, err
}

// lockForCommand loads an item FOR UPDATE and enforces the caller's legal entity and expected version.
func lockForCommand(ctx context.Context, tx pgx.Tx, c kit.Caller, openItemID string, expected *int) (*OpenItem, error) {
	o, err := getItem(ctx, tx, c.TenantID, openItemID, true)
	if err != nil {
		return nil, err
	}
	if c.LegalEntityID != "" && o.LegalEntityID != c.LegalEntityID {
		return nil, notFound() // a body entity that disagrees with the stored one never reveals the item
	}
	if expected != nil && o.Version != *expected {
		return nil, kit.ErrStaleVersion
	}
	return o, nil
}

func addHistory(ctx context.Context, tx pgx.Tx, c kit.Caller, now time.Time, o *OpenItem, event, fromState, reason, evidence string, detail map[string]any) error {
	var d []byte
	if detail != nil {
		d, _ = json.Marshal(detail)
	}
	_, err := tx.Exec(ctx, `INSERT INTO ar07_item_history (history_id, tenant_id, legal_entity_id, open_item_id, event, from_state, to_state, residual_after,
			version_after, actor_id, reason_code, evidence_id, detail, at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,NULLIF($11,''),NULLIF($12,''),$13,$14)`,
		uuid.NewString(), o.TenantID, o.LegalEntityID, o.OpenItemID, event, fromState, o.State, o.Residual, o.Version, c.PrincipalID, reason, evidence, d, now)
	return err
}

func audit(ctx context.Context, tx pgx.Tx, c kit.Caller, command string, before, after *OpenItem, reason, evidence string, detail map[string]any) error {
	c.LegalEntityID = after.LegalEntityID
	var b any
	if before != nil {
		b = snapshot(before)
	}
	return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateOpenItem, AggregateID: after.OpenItemID, Command: command,
		ReasonCode: reason, EvidenceID: evidence, Before: b, After: snapshot(after), Detail: detail})
}

func snapshot(o *OpenItem) map[string]any {
	return map[string]any{"state": o.State, "residual_amount": o.Residual.String(), "original_amount": o.OriginalAmount.String(),
		"disputed": o.Disputed, "collection_hold": o.CollectionHold, "closed": o.ClosedAt != nil, "version": o.Version}
}

func emit(ctx context.Context, tx pgx.Tx, c kit.Caller, o *OpenItem, event string, extra map[string]any) error {
	c.LegalEntityID = o.LegalEntityID
	p := map[string]any{"open_item_id": o.OpenItemID, "source_document_id": o.SourceDocumentID, "document_kind": o.DocumentKind,
		"customer_party_id": o.CustomerPartyID, "currency": o.Currency, "original_amount": o.OriginalAmount.String(),
		"residual_amount": o.Residual.String(), "state": o.State, "version": o.Version, "correlation_id": c.CorrelationID}
	for k, v := range extra {
		p[k] = v
	}
	return kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateOpenItem, AggregateID: o.OpenItemID, EventType: event, Payload: p})
}

// createOpenItem inserts the receivable for one issued document inside tx. A replay for the same document (same
// customer, currency and amount) returns the existing item; a different shape for the same document is a conflict.
func (m *Module) createOpenItem(ctx context.Context, tx pgx.Tx, c kit.Caller, in kit.OpenItemFromDocument, calendar string) (*OpenItem, bool, error) {
	if err := ValidateCreate(in); err != nil {
		return nil, false, err
	}
	if c.TenantID == "" || mustUUID("legal_entity_id", c.LegalEntityID) != nil {
		return nil, false, kit.Invalid("tenant and legal entity are required to open a receivable")
	}
	if calendar == "" {
		calendar = DefaultCalendar
	}
	if len(calendar) > 64 {
		return nil, false, kit.Invalid("business_calendar_id is at most 64 characters")
	}
	if ex, err := getItemBySource(ctx, tx, c.TenantID, in.SourceDocumentID); err == nil {
		if ex.LegalEntityID != c.LegalEntityID || ex.CustomerPartyID != in.CustomerPartyID || ex.Currency != in.Currency ||
			ex.OriginalAmount != in.OriginalAmount || ex.DocumentKind != in.DocumentKind {
			return nil, false, kit.Conflict(kit.CodeInvoiceAlreadyIssued, "an open item already exists for this document with different content")
		}
		return ex, true, nil
	} else if !isNotFound(err) {
		return nil, false, err
	}
	now := m.deps.Now()
	id := uuid.NewString()
	_, err := tx.Exec(ctx, `INSERT INTO ar07_open_items (open_item_id, tenant_id, legal_entity_id, document_kind, source_document_id, document_number,
			customer_party_id, currency, original_amount, residual_amount, issue_date, due_date, business_calendar_id, state, version, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,$11,$12,'OPEN',1,$13,$14,$14)`,
		id, c.TenantID, c.LegalEntityID, in.DocumentKind, in.SourceDocumentID, in.DocumentNumber, in.CustomerPartyID, in.Currency, in.OriginalAmount,
		day(in.IssueDate), day(in.DueDate), calendar, c.PrincipalID, now)
	if err != nil {
		return nil, false, kit.MapPgErr(err)
	}
	o, err := getItem(ctx, tx, c.TenantID, id, false)
	if err != nil {
		return nil, false, err
	}
	if err := addHistory(ctx, tx, c, now, o, "CREATED", "", "", "", map[string]any{"document_kind": o.DocumentKind, "source_document_id": o.SourceDocumentID}); err != nil {
		return nil, false, err
	}
	if err := audit(ctx, tx, c, "CreateOpenItemFromDocument", nil, o, "", "", map[string]any{"source_document_id": o.SourceDocumentID}); err != nil {
		return nil, false, err
	}
	if err := emit(ctx, tx, c, o, EvOpenItemCreated, nil); err != nil {
		return nil, false, err
	}
	return o, false, nil
}

func isNotFound(err error) bool {
	var p *kit.Problem
	return errors.As(err, &p) && p.Code == kit.CodeNotFound
}

// adjustExtra is the evidence a privileged HTTP adjustment carries beyond the port contract.
type adjustExtra struct {
	ReasonCode string
	EvidenceID string
	Expected   *int
	Command    string
}

// applyAdjustment is the ONLY writer of a residual. It locks the item, replays a known event_key as a no-op, refuses a
// residual outside [0, original] (#19), appends the ledger row and moves the residual to its balance_after in ONE
// transaction, then mirrors settlement onto the invoice dimension. replayed is true when nothing changed.
func (m *Module) applyAdjustment(ctx context.Context, tx pgx.Tx, c kit.Caller, in kit.OpenItemAdjustment, x adjustExtra) (*OpenItem, bool, error) {
	if err := ValidateAdjustment(in); err != nil {
		return nil, false, err
	}
	o, err := lockForCommand(ctx, tx, c, in.OpenItemID, nil) // the version is checked after the replay test below
	if err != nil {
		return nil, false, err
	}
	// Replay: the same owning event is a no-op; reuse of the key for a different adjustment is refused.
	var exItem, exKind string
	var exAmt kit.Amount
	switch err := tx.QueryRow(ctx, `SELECT open_item_id::text, kind, amount FROM ar07_adjustments WHERE tenant_id = $1 AND event_key = $2`,
		c.TenantID, in.EventKey).Scan(&exItem, &exKind, &exAmt); {
	case err == nil:
		if exItem != o.OpenItemID || exKind != in.Kind || exAmt != in.Amount {
			return nil, false, kit.Unprocessable(kit.CodeIdempotencyReused, "event_key was already used for a different adjustment")
		}
		return o, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, err
	}
	if x.Expected != nil && o.Version != *x.Expected { // replay answered above even if the version moved on
		return nil, false, kit.ErrStaleVersion
	}
	if o.ClosedAt != nil {
		return nil, false, kit.Conflict(kit.CodeInvalidTransition, "the open item is closed")
	}
	if IsTerminal(o.State) {
		return nil, false, kit.Conflict(kit.CodeInvalidTransition, "a "+o.State+" open item accepts no further adjustment")
	}
	if !in.Amount.FitsCurrency(o.Currency) {
		return nil, false, kit.Invalid("amount has more decimals than the currency allows")
	}
	next, err := ApplyToResidual(o.Residual, o.OriginalAmount, in.Kind, in.Amount)
	if err != nil {
		return nil, false, err
	}
	if in.Kind == kit.AdjWriteOff {
		// SoD: whoever resolved the item's dispute cannot approve its write-off.
		_, resolver, err := disputeParticipants(ctx, tx, c.TenantID, o.OpenItemID)
		if err != nil {
			return nil, false, err
		}
		if resolver != "" && resolver == c.PrincipalID {
			return nil, false, kit.Forbidden(kit.CodeSegregationOfDuties, "the dispute resolver cannot approve the write-off of the same receivable")
		}
	}
	sign, _ := adjustmentSign(in.Kind)
	effect := in.Amount
	if sign < 0 {
		effect = in.Amount.Neg()
	}
	now := m.deps.Now()
	before := *o
	newState := DeriveState(next, o.OriginalAmount, in.Kind)
	newVersion := o.Version + 1
	if _, err := tx.Exec(ctx, `INSERT INTO ar07_adjustments (adjustment_id, tenant_id, legal_entity_id, open_item_id, kind, amount, signed_effect, balance_before, balance_after,
			source_ref_type, source_ref_id, event_key, reason, reason_code, evidence_id, actor_id, version_after, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),NULLIF($15,''),$16,$17,$18)`,
		uuid.NewString(), c.TenantID, o.LegalEntityID, o.OpenItemID, in.Kind, in.Amount, effect, o.Residual, next,
		in.SourceRefType, in.SourceRefID, in.EventKey, in.Reason, x.ReasonCode, x.EvidenceID, c.PrincipalID, newVersion, now); err != nil {
		return nil, false, kit.MapPgErr(err)
	}
	tag, err := tx.Exec(ctx, `UPDATE ar07_open_items SET residual_amount = $3, state = $4, version = $5, updated_at = $6
		WHERE tenant_id = $1 AND open_item_id = $2 AND version = $7`, c.TenantID, o.OpenItemID, next, newState, newVersion, now, o.Version)
	if err != nil {
		return nil, false, kit.MapPgErr(err)
	}
	if tag.RowsAffected() != 1 {
		return nil, false, kit.ErrStaleVersion
	}
	o.Residual, o.State, o.Version, o.UpdatedAt = next, newState, newVersion, now
	cmd := x.Command
	if cmd == "" {
		cmd = "ApplyReceivableAdjustment"
	}
	detail := map[string]any{"kind": in.Kind, "amount": in.Amount.String(), "source_ref_type": in.SourceRefType, "source_ref_id": in.SourceRefID, "event_key": in.EventKey}
	if err := addHistory(ctx, tx, c, now, o, "ADJUSTED", before.State, x.ReasonCode, x.EvidenceID, detail); err != nil {
		return nil, false, err
	}
	if err := audit(ctx, tx, c, cmd, &before, o, x.ReasonCode, x.EvidenceID, detail); err != nil {
		return nil, false, err
	}
	ev := map[string]any{"kind": in.Kind, "adjustment_amount": in.Amount.String(), "source_ref_type": in.SourceRefType, "source_ref_id": in.SourceRefID, "event_key": in.EventKey}
	switch {
	case in.Kind == kit.AdjWriteOff:
		err = emit(ctx, tx, c, o, EvWrittenOff, ev)
	case sign > 0:
		err = emit(ctx, tx, c, o, EvReopened, ev)
	case next.IsZero():
		err = emit(ctx, tx, c, o, EvSettled, ev)
	default:
		err = emit(ctx, tx, c, o, EvPartiallySettled, ev)
	}
	if err != nil {
		return nil, false, err
	}
	if m.invoices != nil && o.DocumentKind == kit.DocInvoice {
		if err := m.invoices.SetSettlementState(ctx, tx, c, o.SourceDocumentID, SettlementDimension(next, o.OriginalAmount, in.Kind)); err != nil {
			return nil, false, err
		}
	}
	return o, false, nil
}

// disputeParticipants returns who opened and who resolved the item's latest dispute ("" when none / unresolved).
func disputeParticipants(ctx context.Context, q querier, tenantID, openItemID string) (opener, resolver string, err error) {
	var res *string
	e := q.QueryRow(ctx, `SELECT opened_by, resolved_by FROM ar07_disputes WHERE tenant_id = $1 AND open_item_id = $2
		ORDER BY opened_at DESC, dispute_id DESC LIMIT 1`, tenantID, openItemID).Scan(&opener, &res)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", "", nil
	}
	if e != nil {
		return "", "", e
	}
	if res != nil {
		resolver = *res
	}
	return opener, resolver, nil
}

func (m *Module) openDispute(ctx context.Context, tx pgx.Tx, c kit.Caller, openItemID string, expected *int, reason, evidence string) (*OpenItem, error) {
	o, err := lockForCommand(ctx, tx, c, openItemID, expected)
	if err != nil {
		return nil, err
	}
	if o.Disputed {
		return nil, kit.Conflict(kit.CodeInvalidTransition, "the open item is already disputed")
	}
	if !o.IsLive() {
		return nil, kit.Conflict(kit.CodeInvalidTransition, "only an open item with a residual can be disputed")
	}
	now := m.deps.Now()
	before := *o
	if _, err := tx.Exec(ctx, `INSERT INTO ar07_disputes (dispute_id, tenant_id, legal_entity_id, open_item_id, status, reason_code, evidence_id, opened_by, opened_at)
		VALUES ($1,$2,$3,$4,'OPEN',$5,$6,$7,$8)`, uuid.NewString(), c.TenantID, o.LegalEntityID, o.OpenItemID, reason, evidence, c.PrincipalID, now); err != nil {
		return nil, kit.MapPgErr(err)
	}
	if err := m.setFlags(ctx, tx, c, o, now, map[string]any{"disputed": true}); err != nil {
		return nil, err
	}
	if err := m.afterFlag(ctx, tx, c, &before, o, now, "DISPUTE_OPENED", "OpenDispute", EvDisputed, reason, evidence, map[string]any{"dispute_status": "OPEN"}); err != nil {
		return nil, err
	}
	if m.invoices != nil && o.DocumentKind == kit.DocInvoice {
		if err := m.invoices.SetDisputeState(ctx, tx, c, o.SourceDocumentID, "DISPUTED"); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (m *Module) resolveDispute(ctx context.Context, tx pgx.Tx, c kit.Caller, openItemID string, expected *int, resolution, evidence string) (*OpenItem, error) {
	o, err := lockForCommand(ctx, tx, c, openItemID, expected)
	if err != nil {
		return nil, err
	}
	if !o.Disputed {
		return nil, kit.Conflict(kit.CodeInvalidTransition, "the open item has no open dispute")
	}
	now := m.deps.Now()
	before := *o
	tag, err := tx.Exec(ctx, `UPDATE ar07_disputes SET status = 'RESOLVED', resolution_code = $3, resolution_evidence_id = $4, resolved_by = $5, resolved_at = $6
		WHERE tenant_id = $1 AND open_item_id = $2 AND status = 'OPEN'`, c.TenantID, o.OpenItemID, resolution, evidence, c.PrincipalID, now)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	if tag.RowsAffected() != 1 {
		return nil, kit.Conflict(kit.CodeInvalidTransition, "the open item has no open dispute")
	}
	if err := m.setFlags(ctx, tx, c, o, now, map[string]any{"disputed": false}); err != nil {
		return nil, err
	}
	if err := m.afterFlag(ctx, tx, c, &before, o, now, "DISPUTE_RESOLVED", "ResolveDispute", EvDisputeResolved, resolution, evidence, map[string]any{"resolution_code": resolution}); err != nil {
		return nil, err
	}
	if m.invoices != nil && o.DocumentKind == kit.DocInvoice {
		if err := m.invoices.SetDisputeState(ctx, tx, c, o.SourceDocumentID, "NONE"); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (m *Module) setHold(ctx context.Context, tx pgx.Tx, c kit.Caller, openItemID string, expected *int, place bool, reason, evidence string) (*OpenItem, error) {
	o, err := lockForCommand(ctx, tx, c, openItemID, expected)
	if err != nil {
		return nil, err
	}
	now := m.deps.Now()
	before := *o
	if place {
		if o.CollectionHold {
			return nil, kit.Conflict(kit.CodeInvalidTransition, "the open item already has a collection hold")
		}
		if !o.IsLive() {
			return nil, kit.Conflict(kit.CodeInvalidTransition, "only an open item with a residual can be held")
		}
		if err := m.setFlags(ctx, tx, c, o, now, map[string]any{"collection_hold": true, "hold_reason": reason}); err != nil {
			return nil, err
		}
		return o, m.afterFlag(ctx, tx, c, &before, o, now, "HOLD_PLACED", "PlaceCollectionHold", EvHoldPlaced, reason, evidence, nil)
	}
	if !o.CollectionHold {
		return nil, kit.Conflict(kit.CodeInvalidTransition, "the open item has no collection hold")
	}
	if err := m.setFlags(ctx, tx, c, o, now, map[string]any{"collection_hold": false, "hold_reason": ""}); err != nil {
		return nil, err
	}
	return o, m.afterFlag(ctx, tx, c, &before, o, now, "HOLD_RELEASED", "ReleaseCollectionHold", EvHoldReleased, reason, evidence, nil)
}

// setFlags writes the orthogonal flags (never the residual) and bumps the version.
func (m *Module) setFlags(ctx context.Context, tx pgx.Tx, c kit.Caller, o *OpenItem, now time.Time, set map[string]any) error {
	if v, ok := set["disputed"]; ok {
		o.Disputed = v.(bool)
	}
	if v, ok := set["collection_hold"]; ok {
		o.CollectionHold = v.(bool)
		o.HoldReason, _ = set["hold_reason"].(string)
	}
	tag, err := tx.Exec(ctx, `UPDATE ar07_open_items SET disputed = $3, collection_hold = $4, hold_reason = NULLIF($5,''), version = version + 1, updated_at = $6
		WHERE tenant_id = $1 AND open_item_id = $2 AND version = $7`, c.TenantID, o.OpenItemID, o.Disputed, o.CollectionHold, o.HoldReason, now, o.Version)
	if err != nil {
		return kit.MapPgErr(err)
	}
	if tag.RowsAffected() != 1 {
		return kit.ErrStaleVersion
	}
	o.Version++
	o.UpdatedAt = now
	return nil
}

func (m *Module) afterFlag(ctx context.Context, tx pgx.Tx, c kit.Caller, before, o *OpenItem, now time.Time, hist, command, event, reason, evidence string, extra map[string]any) error {
	if err := addHistory(ctx, tx, c, now, o, hist, before.State, reason, evidence, extra); err != nil {
		return err
	}
	if err := audit(ctx, tx, c, command, before, o, reason, evidence, extra); err != nil {
		return err
	}
	ev := map[string]any{"reason_code": reason, "evidence_id": evidence}
	for k, v := range extra {
		ev[k] = v
	}
	return emit(ctx, tx, c, o, event, ev)
}

func (m *Module) closeItem(ctx context.Context, tx pgx.Tx, c kit.Caller, openItemID string, expected *int, reason, evidence string) (*OpenItem, error) {
	o, err := lockForCommand(ctx, tx, c, openItemID, expected)
	if err != nil {
		return nil, err
	}
	switch {
	case o.ClosedAt != nil:
		return nil, kit.Conflict(kit.CodeInvalidTransition, "the open item is already closed")
	case o.Residual.IsPositive():
		return nil, kit.Unprocessable(kit.CodeReceivableResidualInvalid, "an open item can be closed only when its residual is zero")
	case o.Disputed:
		return nil, kit.Conflict(kit.CodeInvalidTransition, "resolve the dispute before closing the open item")
	}
	now := m.deps.Now()
	before := *o
	if _, err := tx.Exec(ctx, `UPDATE ar07_open_items SET closed_at = $3, collection_hold = FALSE, hold_reason = NULL, version = version + 1, updated_at = $3
		WHERE tenant_id = $1 AND open_item_id = $2`, c.TenantID, o.OpenItemID, now); err != nil {
		return nil, kit.MapPgErr(err)
	}
	o.ClosedAt, o.CollectionHold, o.HoldReason, o.Version, o.UpdatedAt = &now, false, "", o.Version+1, now
	return o, m.afterFlag(ctx, tx, c, &before, o, now, "CLOSED", "CloseOpenItem", EvClosed, reason, evidence, nil)
}

func loadAdjustments(ctx context.Context, q querier, tenantID, openItemID string) ([]Adjustment, error) {
	rows, err := q.Query(ctx, `SELECT adjustment_id::text, seq, open_item_id::text, kind, amount, signed_effect, balance_before, balance_after, source_ref_type, source_ref_id,
			event_key, reason, COALESCE(reason_code,''), COALESCE(evidence_id,''), actor_id, version_after, recorded_at
		FROM ar07_adjustments WHERE tenant_id = $1 AND open_item_id = $2 ORDER BY seq`, tenantID, openItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Adjustment{}
	for rows.Next() {
		var a Adjustment
		if err := rows.Scan(&a.AdjustmentID, &a.Seq, &a.OpenItemID, &a.Kind, &a.Amount, &a.SignedEffect, &a.BalanceBefore, &a.BalanceAfter, &a.SourceRefType,
			&a.SourceRefID, &a.EventKey, &a.Reason, &a.ReasonCode, &a.EvidenceID, &a.ActorID, &a.VersionAfter, &a.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func loadHistory(ctx context.Context, q querier, tenantID, openItemID string) ([]HistoryEvent, error) {
	rows, err := q.Query(ctx, `SELECT seq, event, COALESCE(from_state,''), COALESCE(to_state,''), residual_after, version_after, actor_id, COALESCE(reason_code,''),
			COALESCE(evidence_id,''), detail, at FROM ar07_item_history WHERE tenant_id = $1 AND open_item_id = $2 ORDER BY seq`, tenantID, openItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEvent{}
	for rows.Next() {
		var h HistoryEvent
		var d []byte
		if err := rows.Scan(&h.Seq, &h.Event, &h.FromState, &h.ToState, &h.ResidualAfter, &h.VersionAfter, &h.ActorID, &h.ReasonCode, &h.EvidenceID, &d, &h.At); err != nil {
			return nil, err
		}
		if len(d) > 0 {
			_ = json.Unmarshal(d, &h.Detail)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func loadDisputes(ctx context.Context, q querier, tenantID, openItemID string) ([]Dispute, error) {
	rows, err := q.Query(ctx, `SELECT dispute_id::text, status, reason_code, evidence_id, opened_by, opened_at, COALESCE(resolution_code,''),
			COALESCE(resolution_evidence_id,''), COALESCE(resolved_by,''), resolved_at FROM ar07_disputes
		WHERE tenant_id = $1 AND open_item_id = $2 ORDER BY opened_at, dispute_id`, tenantID, openItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dispute{}
	for rows.Next() {
		var d Dispute
		if err := rows.Scan(&d.DisputeID, &d.Status, &d.ReasonCode, &d.EvidenceID, &d.OpenedBy, &d.OpenedAt, &d.ResolutionCode, &d.ResolutionEvidenceID, &d.ResolvedBy, &d.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
