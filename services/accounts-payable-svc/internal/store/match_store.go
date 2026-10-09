package store

// AP-06 persistence. Every command runs in ONE transaction that locks the invoice
// (MutateInTx), writes the run / exception rows, updates the invoice's match
// dimension and writes history + outbox events. All queries filter on tenant
// explicitly; row-level security is the backstop, not the only isolation.

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"

	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

func isNotFoundErr(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidIdentifier) || isInvalidUUID(err)
}

func invoiceFinalForMatch(inv *domain.VendorInvoice) error {
	switch {
	case inv.ApprovalState == domain.ApprovalApproved:
		return domain.ErrMatchInvoiceFinal
	case inv.IntakeState == domain.IntakeQuarantined, inv.IntakeState == domain.IntakeRejected:
		return domain.ErrMatchInvoiceState
	}
	return nil
}

// ── policy ──────────────────────────────────────────────────────────────────

// GetMatchPolicy returns one version, or the latest when version == 0. A legal
// entity with no configured policy yields domain.ErrMatchPolicyNotFound: the
// caller then applies domain.DefaultMatchPolicy (strict, zero tolerance).
func (s *PgStore) GetMatchPolicy(ctx context.Context, tenantID, legalEntityID string, version int) (*domain.MatchPolicy, error) {
	var p domain.MatchPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q := `SELECT legal_entity_id::text, policy_version, mode, qty_tolerance_pct::float8, price_tolerance_pct::float8, amount_tolerance_abs::float8, reason, created_by, created_at
			FROM match_policy_versions WHERE tenant_id = $1 AND legal_entity_id = $2`
		args := []any{tenantID, legalEntityID}
		if version > 0 {
			q += ` AND policy_version = $3`
			args = append(args, version)
		} else {
			q += ` ORDER BY policy_version DESC LIMIT 1`
		}
		return tx.QueryRow(ctx, q, args...).Scan(&p.LegalEntityID, &p.PolicyVersion, &p.Mode, &p.QtyTolerancePct,
			&p.PriceTolerancePct, &p.AmountToleranceAbs, &p.Reason, &p.CreatedBy, &p.CreatedAt)
	})
	if isNotFoundErr(err) {
		return nil, domain.ErrMatchPolicyNotFound
	}
	if err != nil {
		return nil, err
	}
	p.TenantID = tenantID
	return &p, nil
}

// CreateMatchPolicy appends the next policy version. Versions are immutable
// (append-only trigger); changing a tolerance is always a NEW version.
func (s *PgStore) CreateMatchPolicy(ctx context.Context, tenantID string, p domain.MatchPolicy) (*domain.MatchPolicy, error) {
	if err := p.Validate(); err != nil {
		return nil, errors.Join(domain.ErrMatchPolicyInvalid, err)
	}
	var out domain.MatchPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tenantID + "|" + p.LegalEntityID))
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(h.Sum64())); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO match_policy_versions (tenant_id, legal_entity_id, policy_version, mode, qty_tolerance_pct, price_tolerance_pct, amount_tolerance_abs, reason, created_by)
			VALUES ($1, $2, COALESCE((SELECT MAX(policy_version) FROM match_policy_versions WHERE tenant_id = $1 AND legal_entity_id = $2), 0) + 1, $3, $4, $5, $6, $7, $8)
			RETURNING legal_entity_id::text, policy_version, mode, qty_tolerance_pct::float8, price_tolerance_pct::float8, amount_tolerance_abs::float8, reason, created_by, created_at`,
			tenantID, p.LegalEntityID, p.Mode, p.QtyTolerancePct, p.PriceTolerancePct, p.AmountToleranceAbs, p.Reason, p.CreatedBy,
		).Scan(&out.LegalEntityID, &out.PolicyVersion, &out.Mode, &out.QtyTolerancePct, &out.PriceTolerancePct,
			&out.AmountToleranceAbs, &out.Reason, &out.CreatedBy, &out.CreatedAt)
	})
	if isInvalidUUID(err) {
		return nil, domain.ErrInvalidIdentifier
	}
	if err != nil {
		return nil, err
	}
	out.TenantID = tenantID
	return &out, nil
}

// ── run persistence ─────────────────────────────────────────────────────────

const runColumns = `run_id::text, legal_entity_id::text, invoice_id::text, run_number, result, mode, policy_version, policy_snapshot,
	purchase_order_id, po_revision, invoice_version, input_hash, totals, requested_by, correlation_id, created_at,
	superseded_at, superseded_by_run_id::text, COALESCE(supersede_reason, '')`

func scanRun(row pgx.Row, tenantID string) (*domain.MatchRunRecord, error) {
	var r domain.MatchRunRecord
	var snap, totals []byte
	if err := row.Scan(&r.RunID, &r.LegalEntityID, &r.InvoiceID, &r.RunNumber, &r.Result, &r.Mode, &r.PolicyVersion, &snap,
		&r.PurchaseOrderID, &r.PORevision, &r.InvoiceVersion, &r.InputHash, &totals, &r.RequestedBy, &r.CorrelationID, &r.CreatedAt,
		&r.SupersededAt, &r.SupersededByRunID, &r.SupersedeReason); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(snap, &r.PolicySnapshot)
	_ = json.Unmarshal(totals, &r.Totals)
	r.TenantID = tenantID
	return &r, nil
}

func currentRun(ctx context.Context, tx pgx.Tx, tenantID, invoiceID string) (*domain.MatchRunRecord, error) {
	r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM invoice_match_runs
		WHERE tenant_id = $1 AND invoice_id = $2 AND superseded_at IS NULL FOR UPDATE`, tenantID, invoiceID), tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

const excColumns = `exception_id::text, run_id::text, invoice_id::text, legal_entity_id::text, COALESCE(invoice_line_id::text, ''), COALESCE(po_line_id, ''),
	category, class, waivable, COALESCE(expected::float8, 0), COALESCE(actual::float8, 0), COALESCE(difference::float8, 0), detail, status,
	COALESCE(acknowledged_by, ''), COALESCE(routed_to, ''), COALESCE(routed_by, ''), COALESCE(route_reason, ''),
	COALESCE(resolved_by, ''), COALESCE(resolution_reason, ''), COALESCE(resolution_ref, '')`

func scanException(row pgx.Row) (*domain.MatchExceptionRecord, error) {
	var x domain.MatchExceptionRecord
	if err := row.Scan(&x.ExceptionID, &x.RunID, &x.InvoiceID, &x.LegalEntityID, &x.InvoiceLineID, &x.POLineID,
		&x.Category, &x.Class, &x.Waivable, &x.Expected, &x.Actual, &x.Difference, &x.Detail, &x.Status,
		&x.AcknowledgedBy, &x.RoutedTo, &x.RoutedBy, &x.RouteReason, &x.ResolvedBy, &x.ResolutionReason, &x.ResolutionRef); err != nil {
		return nil, err
	}
	return &x, nil
}

func loadView(ctx context.Context, tx pgx.Tx, tenantID string, run *domain.MatchRunRecord) (*domain.MatchResultView, error) {
	v := &domain.MatchResultView{Run: run, Lines: []domain.MatchLineResult{}, Exceptions: []domain.MatchExceptionRecord{}}
	rows, err := tx.Query(ctx, `SELECT detail FROM invoice_match_lines WHERE tenant_id = $1 AND run_id = $2 ORDER BY line_number, line_result_id`, tenantID, run.RunID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		var l domain.MatchLineResult
		_ = json.Unmarshal(raw, &l)
		v.Lines = append(v.Lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	erows, err := tx.Query(ctx, `SELECT `+excColumns+` FROM invoice_match_exceptions WHERE tenant_id = $1 AND run_id = $2 ORDER BY created_at, exception_id`, tenantID, run.RunID)
	if err != nil {
		return nil, err
	}
	for erows.Next() {
		x, err := scanException(erows)
		if err != nil {
			erows.Close()
			return nil, err
		}
		v.Exceptions = append(v.Exceptions, *x)
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT match_state, match_cleared FROM vendor_invoices WHERE tenant_id = $1 AND invoice_id = $2`, tenantID, run.InvoiceID).
		Scan(&v.InvoiceMatchState, &v.Cleared); err != nil {
		return nil, err
	}
	return v, nil
}

// SaveMatchRun persists an evaluated run. If the live run already holds identical
// evidence (same input hash) nothing is written and that run is returned with
// Created=false; otherwise the live run is superseded and a new one recorded.
func (s *PgStore) SaveMatchRun(ctx context.Context, in domain.SaveMatchRunInput) (*domain.MatchResultView, error) {
	var view *domain.MatchResultView
	err := s.withRLS(ctx, in.TenantID, func(tx pgx.Tx) error {
		var newRunID string
		var replay *domain.MatchRunRecord
		var superseded *domain.MatchRunRecord
		var runNumber int

		inv, err := s.MutateInTx(ctx, tx, in.TenantID, in.InvoiceID, in.ExpectedVersion, func(inv *domain.VendorInvoice) (*domain.Mutation, error) {
			if !inv.RequiresMatch() {
				return nil, domain.ErrMatchNotApplicable
			}
			if err := invoiceFinalForMatch(inv); err != nil {
				return nil, err
			}
			cur, err := currentRun(ctx, tx, in.TenantID, in.InvoiceID)
			if err != nil {
				return nil, err
			}
			if cur != nil && cur.InputHash == in.Outcome.InputHash {
				replay = cur
				return nil, nil // deterministic re-performance on unchanged evidence: no new run
			}
			var maxN int
			if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(run_number), 0) FROM invoice_match_runs WHERE tenant_id = $1 AND invoice_id = $2`, in.TenantID, in.InvoiceID).Scan(&maxN); err != nil {
				return nil, err
			}
			runNumber = maxN + 1
			newRunID = uuid.NewString()
			if cur != nil {
				if _, err := tx.Exec(ctx, `UPDATE invoice_match_runs SET superseded_at = NOW(), superseded_by_run_id = $3, supersede_reason = $4
					WHERE tenant_id = $1 AND run_id = $2`, in.TenantID, cur.RunID, newRunID, "re-performed on changed evidence"); err != nil {
					return nil, mapPgError(err)
				}
				superseded = cur
			}
			snap, _ := json.Marshal(in.Policy)
			totals, _ := json.Marshal(in.Outcome.Totals)
			if _, err := tx.Exec(ctx, `
				INSERT INTO invoice_match_runs (run_id, tenant_id, legal_entity_id, invoice_id, run_number, result, mode, policy_version, policy_snapshot,
					purchase_order_id, po_revision, invoice_version, input_hash, totals, requested_by, correlation_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
				newRunID, in.TenantID, inv.LegalEntityID, inv.InvoiceID, runNumber, string(in.Outcome.Result), string(in.Policy.Mode), in.Policy.PolicyVersion, snap,
				in.PurchaseOrderID, in.PORevision, inv.Version, in.Outcome.InputHash, totals, in.Actor, in.CorrelationID); err != nil {
				return nil, mapPgError(err)
			}
			lineIDs := map[string]struct{}{}
			for _, l := range in.Outcome.Lines {
				detail, _ := json.Marshal(l)
				var lineID any
				if l.InvoiceLineID != "" {
					lineID = l.InvoiceLineID
					lineIDs[l.InvoiceLineID] = struct{}{}
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO invoice_match_lines (line_result_id, tenant_id, run_id, invoice_id, invoice_line_id, line_number, po_line_id, result, detail)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
					uuid.NewString(), in.TenantID, newRunID, inv.InvoiceID, lineID, l.LineNumber, nullable(l.POLineID), string(l.Result), detail); err != nil {
					return nil, mapPgError(err)
				}
			}
			for _, x := range in.Outcome.Exceptions {
				var lineID any
				if x.InvoiceLineID != "" {
					lineID = x.InvoiceLineID
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO invoice_match_exceptions (exception_id, tenant_id, legal_entity_id, run_id, invoice_id, invoice_line_id, po_line_id,
						category, class, waivable, expected, actual, difference, detail)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
					uuid.NewString(), in.TenantID, inv.LegalEntityID, newRunID, inv.InvoiceID, lineID, nullable(x.POLineID),
					x.Category, x.Class, x.Waivable, x.Expected, x.Actual, x.Difference, x.Detail); err != nil {
					return nil, mapPgError(err)
				}
			}

			// The invoice's match dimension, written only here and in the exception/supersede commands.
			if err := inv.SetMatch(in.Outcome.Result); err != nil {
				return nil, err
			}
			inv.MatchRequired = true
			inv.MatchCleared = in.Outcome.Cleared()
			inv.MatchRunID = &newRunID

			payload := map[string]any{
				"invoice_id": inv.InvoiceID, "run_id": newRunID, "run_number": runNumber, "result": in.Outcome.Result,
				"policy_version": in.Policy.PolicyVersion, "mode": in.Policy.Mode, "input_hash": in.Outcome.InputHash,
				"purchase_order_id": in.PurchaseOrderID, "exceptions": len(in.Outcome.Exceptions),
			}
			evs := []domain.OutboxEvent{{EventType: domain.EventInvoiceMatchStarted, Payload: payload}}
			if superseded != nil {
				evs = append(evs, domain.OutboxEvent{EventType: domain.EventInvoiceMatchSuperseded, Payload: map[string]any{
					"invoice_id": inv.InvoiceID, "superseded_run_id": superseded.RunID, "superseded_by_run_id": newRunID}})
			}
			if in.Outcome.Cleared() {
				evs = append(evs, domain.OutboxEvent{EventType: domain.EventInvoiceMatched, Payload: payload})
			} else {
				evs = append(evs, domain.OutboxEvent{EventType: domain.EventInvoiceMatchExceptionRaised, Payload: payload})
			}
			return &domain.Mutation{
				Command: in.Command, Actor: in.Actor, CorrelationID: in.CorrelationID,
				Detail: map[string]any{"run_id": newRunID, "run_number": runNumber, "result": in.Outcome.Result, "input_hash": in.Outcome.InputHash},
				Events: evs,
			}, nil
		})
		if err != nil {
			return err
		}
		_ = inv
		run := replay
		created := false
		if run == nil {
			created = true
			if run, err = scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM invoice_match_runs WHERE tenant_id = $1 AND run_id = $2`, in.TenantID, newRunID), in.TenantID); err != nil {
				return err
			}
		}
		if view, err = loadView(ctx, tx, in.TenantID, run); err != nil {
			return err
		}
		view.Created = created
		return nil
	})
	if isNotFoundErr(err) && !errors.Is(err, domain.ErrMatchRunNotFound) {
		return nil, domain.ErrInvoiceNotFound
	}
	if err != nil {
		return nil, err
	}
	return view, nil
}

// SupersedeMatchRun invalidates the live run without starting a new one: the
// invoice returns to NOT_MATCHED and cannot be approved until it is matched again.
func (s *PgStore) SupersedeMatchRun(ctx context.Context, in domain.SupersedeInput) (*domain.VendorInvoice, error) {
	var out *domain.VendorInvoice
	err := s.withRLS(ctx, in.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = s.MutateInTx(ctx, tx, in.TenantID, in.InvoiceID, in.ExpectedVersion, func(inv *domain.VendorInvoice) (*domain.Mutation, error) {
			if err := invoiceFinalForMatch(inv); err != nil {
				return nil, err
			}
			cur, err := currentRun(ctx, tx, in.TenantID, in.InvoiceID)
			if err != nil {
				return nil, err
			}
			if cur == nil {
				return nil, domain.ErrMatchRunNotFound
			}
			if _, err := tx.Exec(ctx, `UPDATE invoice_match_runs SET superseded_at = NOW(), supersede_reason = $3 WHERE tenant_id = $1 AND run_id = $2`,
				in.TenantID, cur.RunID, in.Reason); err != nil {
				return nil, mapPgError(err)
			}
			if err := inv.SetMatch(domain.MatchNotMatched); err != nil {
				return nil, err
			}
			inv.MatchCleared, inv.MatchRunID = false, nil
			return &domain.Mutation{
				Command: "SupersedeMatchRun", Actor: in.Actor, Reason: in.Reason, CorrelationID: in.CorrelationID,
				Detail: map[string]any{"superseded_run_id": cur.RunID},
				Events: []domain.OutboxEvent{{EventType: domain.EventInvoiceMatchSuperseded, Payload: map[string]any{
					"invoice_id": inv.InvoiceID, "superseded_run_id": cur.RunID, "reason": in.Reason}}},
			}, nil
		})
		return err
	})
	if isNotFoundErr(err) && !errors.Is(err, domain.ErrMatchRunNotFound) {
		return nil, domain.ErrInvoiceNotFound
	}
	return out, err
}

// ResolveMatchException applies a human decision to one exception of the LIVE run.
// Approving a variance is the only way an EXCEPTION run ever clears, and only when
// every exception of the run is a waivable variance that has been approved.
func (s *PgStore) ResolveMatchException(ctx context.Context, a domain.ExceptionAction) (*domain.MatchExceptionRecord, *domain.VendorInvoice, error) {
	var invoiceID string
	err := s.withRLS(ctx, a.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT invoice_id::text FROM invoice_match_exceptions WHERE tenant_id = $1 AND exception_id = $2`, a.TenantID, a.ExceptionID).Scan(&invoiceID)
	})
	if isNotFoundErr(err) {
		return nil, nil, domain.ErrMatchExceptionNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	var exc *domain.MatchExceptionRecord
	var invOut *domain.VendorInvoice
	err = s.withRLS(ctx, a.TenantID, func(tx pgx.Tx) error {
		var err error
		invOut, err = s.MutateInTx(ctx, tx, a.TenantID, invoiceID, nil, func(inv *domain.VendorInvoice) (*domain.Mutation, error) {
			if err := invoiceFinalForMatch(inv); err != nil {
				return nil, err
			}
			var superseded *time.Time
			var requester string
			if err := tx.QueryRow(ctx, `
				SELECT r.superseded_at, r.requested_by FROM invoice_match_exceptions e
				JOIN invoice_match_runs r ON r.run_id = e.run_id AND r.tenant_id = e.tenant_id
				WHERE e.tenant_id = $1 AND e.exception_id = $2`, a.TenantID, a.ExceptionID).Scan(&superseded, &requester); err != nil {
				return nil, err
			}
			if superseded != nil {
				return nil, domain.ErrMatchRunSuperseded
			}
			cur, err := scanException(tx.QueryRow(ctx, `SELECT `+excColumns+` FROM invoice_match_exceptions WHERE tenant_id = $1 AND exception_id = $2 FOR UPDATE`, a.TenantID, a.ExceptionID))
			if err != nil {
				return nil, err
			}

			mut := &domain.Mutation{Actor: a.Actor, Reason: a.Reason, CorrelationID: a.CorrelationID, KeepVersion: true,
				Detail: map[string]any{"exception_id": cur.ExceptionID, "category": cur.Category}}
			switch a.Kind {
			case domain.ActionAcknowledge:
				if cur.Status != domain.ExceptionOpen && cur.Status != domain.ExceptionRouted {
					return nil, domain.ErrExceptionTransition
				}
				if _, err := tx.Exec(ctx, `UPDATE invoice_match_exceptions SET status = 'ACKNOWLEDGED', acknowledged_by = $3, acknowledged_at = NOW()
					WHERE tenant_id = $1 AND exception_id = $2`, a.TenantID, a.ExceptionID, a.Actor); err != nil {
					return nil, mapPgError(err)
				}
				mut.Command = "AcknowledgeMatchException"
			case domain.ActionRoute:
				if cur.Status != domain.ExceptionOpen && cur.Status != domain.ExceptionAcknowledged {
					return nil, domain.ErrExceptionTransition
				}
				if _, err := tx.Exec(ctx, `UPDATE invoice_match_exceptions SET status = 'ROUTED', routed_to = $3, routed_by = $4, routed_at = NOW(), route_reason = $5
					WHERE tenant_id = $1 AND exception_id = $2`, a.TenantID, a.ExceptionID, a.RouteTo, a.Actor, a.Reason); err != nil {
					return nil, mapPgError(err)
				}
				mut.Command = "RouteMatchException"
				mut.Detail["routed_to"] = a.RouteTo
			case domain.ActionApproveVariance:
				if cur.Status == domain.ExceptionVarianceApproved {
					return nil, domain.ErrExceptionTransition
				}
				if !cur.Waivable || cur.Class != domain.ClassVariance {
					return nil, domain.ErrExceptionNotWaivable
				}
				if a.Actor == requester || a.Actor == inv.CreatedByPrincipalID {
					return nil, domain.ErrMatchSelfWaiver
				}
				if _, err := tx.Exec(ctx, `UPDATE invoice_match_exceptions SET status = 'VARIANCE_APPROVED', resolved_by = $3, resolved_at = NOW(), resolution_reason = $4, resolution_ref = $5
					WHERE tenant_id = $1 AND exception_id = $2`, a.TenantID, a.ExceptionID, a.Actor, a.Reason, nullable(a.Ref)); err != nil {
					return nil, mapPgError(err)
				}
				mut.Command = "RecordApprovedVariance"
				mut.Events = []domain.OutboxEvent{{EventType: domain.EventInvoiceMatchVarianceApproved, Payload: map[string]any{
					"invoice_id": inv.InvoiceID, "run_id": cur.RunID, "exception_id": cur.ExceptionID, "category": cur.Category,
					"difference": cur.Difference, "reason": a.Reason, "reference": a.Ref}}}
				var open int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM invoice_match_exceptions WHERE tenant_id = $1 AND run_id = $2 AND status <> 'VARIANCE_APPROVED'`, a.TenantID, cur.RunID).Scan(&open); err != nil {
					return nil, err
				}
				if open == 0 {
					// Every finding of the live run is a waivable variance with a recorded,
					// independently approved decision: the invoice may now go to approval.
					if err := inv.SetMatch(domain.MatchWithinTolerance); err != nil {
						return nil, err
					}
					inv.MatchCleared = true
					mut.KeepVersion = false
					mut.Events = append(mut.Events, domain.OutboxEvent{EventType: domain.EventInvoiceMatched, Payload: map[string]any{
						"invoice_id": inv.InvoiceID, "run_id": cur.RunID, "result": domain.MatchWithinTolerance, "basis": "approved_variances"}})
				}
			default:
				return nil, domain.ErrExceptionTransition
			}
			return mut, nil
		})
		if err != nil {
			return err
		}
		exc, err = scanException(tx.QueryRow(ctx, `SELECT `+excColumns+` FROM invoice_match_exceptions WHERE tenant_id = $1 AND exception_id = $2`, a.TenantID, a.ExceptionID))
		return err
	})
	if isNotFoundErr(err) {
		return nil, nil, domain.ErrMatchExceptionNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return exc, invOut, nil
}

// ── reads ───────────────────────────────────────────────────────────────────

// GetMatchResult returns the invoice's latest run (live, or the last superseded
// one when the live run was invalidated) with its line facts and exceptions.
func (s *PgStore) GetMatchResult(ctx context.Context, tenantID, invoiceID string) (*domain.MatchResultView, error) {
	var v *domain.MatchResultView
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM invoice_match_runs
			WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY run_number DESC LIMIT 1`, tenantID, invoiceID), tenantID)
		if err != nil {
			return err
		}
		v, err = loadView(ctx, tx, tenantID, run)
		return err
	})
	if isNotFoundErr(err) {
		return nil, domain.ErrMatchRunNotFound
	}
	return v, err
}

// ListMatchRuns is the supersession chain, newest first.
func (s *PgStore) ListMatchRuns(ctx context.Context, tenantID, invoiceID string) ([]domain.MatchRunRecord, error) {
	out := []domain.MatchRunRecord{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM invoice_match_runs WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY run_number DESC`, tenantID, invoiceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRun(rows, tenantID)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	if isNotFoundErr(err) {
		return []domain.MatchRunRecord{}, nil
	}
	return out, err
}

// ListMatchExceptions lists exceptions of LIVE runs for a legal entity.
func (s *PgStore) ListMatchExceptions(ctx context.Context, f domain.ExceptionFilter) ([]domain.MatchExceptionRecord, error) {
	out := []domain.MatchExceptionRecord{}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	err := s.withRLS(ctx, f.TenantID, func(tx pgx.Tx) error {
		q := `SELECT ` + excColumns + ` FROM invoice_match_exceptions
			WHERE tenant_id = $1 AND legal_entity_id = $2
			  AND run_id IN (SELECT run_id FROM invoice_match_runs WHERE tenant_id = $1 AND superseded_at IS NULL)`
		args := []any{f.TenantID, f.LegalEntityID}
		if f.Status != "" {
			args = append(args, f.Status)
			q += ` AND status = $3`
		}
		if f.InvoiceID != "" {
			args = append(args, f.InvoiceID)
			q += ` AND invoice_id = $` + string(rune('0'+len(args)))
		}
		args = append(args, limit)
		q += ` ORDER BY created_at, exception_id LIMIT $` + string(rune('0'+len(args)))
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			x, err := scanException(rows)
			if err != nil {
				return err
			}
			out = append(out, *x)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return []domain.MatchExceptionRecord{}, nil
	}
	return out, err
}

// GetMatchException returns one exception.
func (s *PgStore) GetMatchException(ctx context.Context, tenantID, exceptionID string) (*domain.MatchExceptionRecord, error) {
	var x *domain.MatchExceptionRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		x, err = scanException(tx.QueryRow(ctx, `SELECT `+excColumns+` FROM invoice_match_exceptions WHERE tenant_id = $1 AND exception_id = $2`, tenantID, exceptionID))
		return err
	})
	if isNotFoundErr(err) {
		return nil, domain.ErrMatchExceptionNotFound
	}
	return x, err
}

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
