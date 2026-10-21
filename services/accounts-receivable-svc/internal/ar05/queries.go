package ar05

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// params merges the query string with, for POST, the top-level scalar fields of a JSON body (the spec wireframe allows
// GET or POST for queries).
func params(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	if r.Method == http.MethodPost && r.Body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, kit.MaxBodyBytes))
		if err == nil && len(raw) > 0 {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				for k, v := range m {
					switch t := v.(type) {
					case string:
						out[k] = t
					case float64:
						out[k] = strconv.FormatFloat(t, 'f', -1, 64)
					}
				}
			}
		}
	}
	return out
}

// resolveSubmission finds a submission by submission_id, or the newest one of document_id (+ document_type, profile_code).
func resolveSubmission(ctx context.Context, tx pgx.Tx, tenantID string, p map[string]string) (*Submission, error) {
	if id := p["submission_id"]; id != "" {
		return getSubmission(ctx, tx, tenantID, id, false)
	}
	if aid := p["external_attempt_id"]; aid != "" {
		a, err := getAttempt(ctx, tx, tenantID, aid)
		if err != nil {
			return nil, err
		}
		return getSubmission(ctx, tx, tenantID, a.SubmissionID, false)
	}
	docID := p["document_id"]
	if docID == "" {
		docID = p["invoice_id"]
	}
	if docID == "" {
		return nil, kit.Invalid("submission_id, external_attempt_id or document_id is required")
	}
	if err := mustUUID("document_id", docID); err != nil {
		return nil, err
	}
	docType := upperTrim(p["document_type"])
	if docType == "" {
		docType = DocInvoice
	}
	q := `SELECT ` + subCols + ` FROM ar05_einvoice_submissions WHERE tenant_id = $1 AND document_type = $2 AND document_id = $3`
	args := []any{tenantID, docType, docID}
	if pc := p["profile_code"]; pc != "" {
		q += ` AND profile_code = $4`
		args = append(args, pc)
	}
	q += ` ORDER BY created_at DESC LIMIT 1`
	s, err := scanSubmission(tx.QueryRow(ctx, q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("no e-invoice submission for this document")
	}
	return s, kit.MapPgErr(err)
}

func (m *Module) getEInvoiceStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out map[string]any
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		s, err := resolveSubmission(r.Context(), tx, c.TenantID, p)
		if err != nil {
			return err
		}
		att, err := latestAttempt(r.Context(), tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		val, err := latestValidation(r.Context(), tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		ack, err := latestAck(r.Context(), tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		out = map[string]any{"submission": s, "available_actions": available(s.State), "latest_attempt": att, "latest_validation": val, "latest_acknowledgement": ack,
			"einvoice_dimension": s.State.Dimension(true)}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) getSubmissionAttempt(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out any
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		if aid := p["external_attempt_id"]; aid != "" {
			a, err := getAttempt(r.Context(), tx, c.TenantID, aid)
			if err != nil {
				return err
			}
			if a.Results, err = listResults(r.Context(), tx, c.TenantID, aid); err != nil {
				return err
			}
			out = a
			return nil
		}
		s, err := resolveSubmission(r.Context(), tx, c.TenantID, p)
		if err != nil {
			return err
		}
		as, err := listAttempts(r.Context(), tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		for i := range as {
			if as[i].Results, err = listResults(r.Context(), tx, c.TenantID, as[i].ExternalAttemptID); err != nil {
				return err
			}
		}
		out = map[string]any{"submission_id": s.SubmissionID, "attempts": as}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) getValidationErrors(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out map[string]any
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		s, err := resolveSubmission(r.Context(), tx, c.TenantID, p)
		if err != nil {
			return err
		}
		v, err := latestValidation(r.Context(), tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		if v == nil {
			return kit.NotFound("the submission has not been validated yet")
		}
		out = map[string]any{"submission_id": s.SubmissionID, "validation_status": s.ValidationStatus, "validation": v, "errors": v.Errors}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) getAcknowledgement(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out map[string]any
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		s, err := resolveSubmission(r.Context(), tx, c.TenantID, p)
		if err != nil {
			return err
		}
		all, err := listAcks(r.Context(), tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return kit.NotFound("no acknowledgement has been recorded for this submission")
		}
		out = map[string]any{"submission_id": s.SubmissionID, "state": s.State, "latest": all[len(all)-1], "acknowledgements": all}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) listSubmissionHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	limit := 50
	if v := p["limit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			m.fail(w, kit.Invalid("limit must be a positive integer"))
			return
		}
		limit = n
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	var out map[string]any
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		ctx := r.Context()
		if p["submission_id"] == "" && p["external_attempt_id"] == "" {
			// The resubmission chain of a document, newest first.
			docID := p["document_id"]
			if docID == "" {
				docID = p["invoice_id"]
			}
			if err := mustUUID("document_id", docID); err != nil {
				return err
			}
			docType := upperTrim(p["document_type"])
			if docType == "" {
				docType = DocInvoice
			}
			rows, err := tx.Query(ctx, `SELECT `+subCols+` FROM ar05_einvoice_submissions WHERE tenant_id = $1 AND document_type = $2 AND document_id = $3
				ORDER BY created_at DESC, submission_id LIMIT $4`, c.TenantID, docType, docID, limit)
			if err != nil {
				return kit.MapPgErr(err)
			}
			defer rows.Close()
			list := []*Submission{}
			for rows.Next() {
				s, err := scanSubmission(rows)
				if err != nil {
					return err
				}
				list = append(list, s)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			out = map[string]any{"document_id": docID, "submissions": list}
			return nil
		}
		s, err := resolveSubmission(ctx, tx, c.TenantID, p)
		if err != nil {
			return err
		}
		hs, err := listHistory(ctx, tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		as, err := listAttempts(ctx, tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		for i := range as {
			if as[i].Results, err = listResults(ctx, tx, c.TenantID, as[i].ExternalAttemptID); err != nil {
				return err
			}
		}
		acks, err := listAcks(ctx, tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		recs, err := listReconciliations(ctx, tx, c.TenantID, s.SubmissionID)
		if err != nil {
			return err
		}
		out = map[string]any{"submission": s, "history": hs, "attempts": as, "acknowledgements": acks, "reconciliations": recs}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}
