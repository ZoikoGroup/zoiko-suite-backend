// BNK-03 Statement Ingestion + BNK-04 Transaction Normalization's own
// HTTP surface, added to this service's existing /v1/banking routes.
package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

const (
	BANKING_STATEMENT_VALIDATE        = "BANKING_STATEMENT_VALIDATE"
	BANKING_TRANSACTION_NORMALIZE     = "BANKING_TRANSACTION_NORMALIZE"
	BANKING_MAPPING_EXCEPTION_APPROVE = "BANKING_MAPPING_EXCEPTION_APPROVE"
	BANKING_MAPPING_MANAGE            = "BANKING_MAPPING_MANAGE"

	// BANK_TRANSACTION_READ is the spec's own BNK-04 read permission (table
	// 24: "bank.transaction.read"), registered in access-control-svc's
	// permission taxonomy by its migration 000022.
	BANK_TRANSACTION_READ = "bank.transaction.read"
)

// BNK0304Handler embeds *Handler — same "separate registration function"
// pattern used for every other capability added to an existing service in
// this build.
type BNK0304Handler struct {
	*Handler
	bnk0304Store store.BNK0304Store
}

// RegisterBNK0304Routes mounts BNK-03/04's routes on r. Called separately
// from RegisterRoutes in cmd/server/main.go.
func RegisterBNK0304Routes(r chi.Router, h *Handler, bnk0304Store store.BNK0304Store) {
	bh := &BNK0304Handler{Handler: h, bnk0304Store: bnk0304Store}
	r.Route("/v1/banking/statements", func(r chi.Router) {
		r.Post("/ingest", bh.IngestStatement)
		r.Post("/{id}/validate", bh.ValidateStatement)
		r.Post("/{id}/accept", bh.AcceptStatement)
		r.Post("/{id}/quarantine", bh.QuarantineStatement)
		r.Post("/{id}/reprocess", bh.ReprocessQuarantinedStatement)
	})
	r.Route("/v1/banking/transactions", func(r chi.Router) {
		r.Post("/normalize", bh.NormalizeTransaction)
		r.Post("/{id}/re-normalize", bh.ReNormalizeTransaction)
		r.Post("/quarantine", bh.QuarantineTransaction)
	})
	r.Post("/v1/banking/transaction-mappings", bh.CreateTransactionMapping)
	r.Route("/v1/banking/canonical-transactions", func(r chi.Router) {
		r.Get("/", bh.ListCanonicalTransactions)
		r.Get("/{id}", bh.GetCanonicalTransaction)
	})
	r.Post("/v1/banking/mapping-exceptions/{id}/approve", bh.ApproveMappingException)
}

// requireLegalEntityAuthz resolves the connection owning a statement (via
// its connection_id) and enforces authz against that connection's legal
// entity — statements have no legal_entity_id of their own, so this is
// the same authority boundary CreateConnection/IngestStatement already
// use elsewhere in this handler package.
func (h *BNK0304Handler) requireLegalEntityAuthz(w http.ResponseWriter, r *http.Request, connectionID, action string) (principalID string, ok bool) {
	conn, err := h.store.GetConnectionByID(r.Context(), connectionID)
	if err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "bank connection not found")
			return "", false
		}
		h.logger.Error("requireLegalEntityAuthz: store unavailable", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to load bank connection")
		return "", false
	}
	principalID, ok = h.requirePrincipal(w, r)
	if !ok {
		return "", false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return "", false
	}
	return principalID, true
}

func (h *BNK0304Handler) writeStatementErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrStatementNotFound):
		writeError(w, http.StatusNotFound, "bank statement not found")
	case errors.Is(err, domain.ErrInvalidStatementTransition):
		writeError(w, http.StatusConflict, "bank statement is not in a state that permits this action")
	default:
		h.logger.Error("bank statement operation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "bank statement operation failed")
	}
}

func (h *BNK0304Handler) writeTransactionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrTransactionNotFound):
		writeError(w, http.StatusNotFound, "canonical transaction not found")
	case errors.Is(err, domain.ErrInvalidTransactionTransition):
		writeError(w, http.StatusConflict, "canonical transaction is not in a state that permits this action")
	case errors.Is(err, domain.ErrMappingExceptionNotFound):
		writeError(w, http.StatusNotFound, "mapping exception not found")
	case errors.Is(err, domain.ErrMappingExceptionNotOpen):
		writeError(w, http.StatusConflict, "mapping exception is not OPEN")
	case errors.Is(err, domain.ErrMappingExceptionSelfApproval):
		writeError(w, http.StatusForbidden, "the principal who raised this exception cannot approve it")
	case errors.Is(err, domain.ErrStatementLineNotFound):
		writeError(w, http.StatusNotFound, "statement line not found")
	case errors.Is(err, domain.ErrTransactionEvidenceMismatch):
		writeError(w, http.StatusUnprocessableEntity, "transaction date, amount or currency does not match the statement line")
	case errors.Is(err, domain.ErrMappingAlreadyExists):
		writeError(w, http.StatusConflict, "a mapping for this bank_code already exists for this tenant")
	default:
		h.logger.Error("bank transaction operation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "bank transaction operation failed")
	}
}

// IngestStatement handles POST /v1/banking/statements/ingest — BNK-03's
// real entry point. Idempotent on (connection_id, content_hash): a
// re-uploaded statement returns the original import, never a duplicate.
func (h *BNK0304Handler) IngestStatement(w http.ResponseWriter, r *http.Request) {
	var req domain.IngestStatementLinesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ConnectionID == "" || req.ContentHash == "" || len(req.Lines) == 0 {
		writeError(w, http.StatusBadRequest, "connection_id, content_hash and at least one line are required")
		return
	}
	principalID, ok := h.requireLegalEntityAuthz(w, r, req.ConnectionID, BANKING_STATEMENT_INGEST)
	if !ok {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	result, err := h.bnk0304Store.IngestStatement(r.Context(), tenantID, req, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.logger.Error("IngestStatement failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to ingest bank statement")
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (h *BNK0304Handler) statementIDAndTenant(r *http.Request) (statementID, tenantID string) {
	return chi.URLParam(r, "id"), middleware.GetTenantID(r.Context())
}

func (h *BNK0304Handler) ValidateStatement(w http.ResponseWriter, r *http.Request) {
	id, tenantID := h.statementIDAndTenant(r)
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if err := h.bnk0304Store.ValidateStatement(r.Context(), tenantID, id); err != nil {
		if errors.Is(err, domain.ErrStatementBalanceMismatch) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"statement_id": id, "status": domain.StatementQuarantine, "reason": err.Error()})
			return
		}
		h.writeStatementErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"statement_id": id, "status": domain.StatementValidating})
}

func (h *BNK0304Handler) AcceptStatement(w http.ResponseWriter, r *http.Request) {
	id, tenantID := h.statementIDAndTenant(r)
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if err := h.bnk0304Store.AcceptStatement(r.Context(), tenantID, id); err != nil {
		h.writeStatementErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"statement_id": id, "status": domain.StatementAccepted})
}

func (h *BNK0304Handler) QuarantineStatement(w http.ResponseWriter, r *http.Request) {
	id, tenantID := h.statementIDAndTenant(r)
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	var req reasonBody
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.bnk0304Store.QuarantineStatement(r.Context(), tenantID, id, req.Reason); err != nil {
		h.writeStatementErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"statement_id": id, "status": domain.StatementQuarantine})
}

func (h *BNK0304Handler) ReprocessQuarantinedStatement(w http.ResponseWriter, r *http.Request) {
	id, tenantID := h.statementIDAndTenant(r)
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if err := h.bnk0304Store.ReprocessQuarantinedStatement(r.Context(), tenantID, id); err != nil {
		h.writeStatementErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"statement_id": id, "status": domain.StatementReceived})
}

func (h *BNK0304Handler) NormalizeTransaction(w http.ResponseWriter, r *http.Request) {
	var req domain.NormalizeTransactionParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.StatementLineID == "" {
		writeError(w, http.StatusBadRequest, "statement_line_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	req.TenantID = middleware.GetTenantID(r.Context())
	req.ActorPrincipalID = principalID
	req.CorrelationID = r.Header.Get("X-Correlation-ID")
	result, err := h.bnk0304Store.NormalizeTransaction(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	if result.Transaction != nil {
		writeJSON(w, http.StatusCreated, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *BNK0304Handler) ReNormalizeTransaction(w http.ResponseWriter, r *http.Request) {
	var req domain.ReNormalizeTransactionParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	req.PriorTransactionID = chi.URLParam(r, "id")
	req.TenantID = middleware.GetTenantID(r.Context())
	req.ActorPrincipalID = principalID
	req.CorrelationID = r.Header.Get("X-Correlation-ID")
	txn, err := h.bnk0304Store.ReNormalizeTransaction(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, txn)
}

func (h *BNK0304Handler) QuarantineTransaction(w http.ResponseWriter, r *http.Request) {
	var req domain.QuarantineTransactionParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.StatementLineID == "" || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "statement_line_id and reason are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	req.TenantID = middleware.GetTenantID(r.Context())
	req.ActorPrincipalID = principalID
	req.CorrelationID = r.Header.Get("X-Correlation-ID")
	exc, err := h.bnk0304Store.QuarantineTransaction(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, exc)
}

// ApproveMappingException is BNK-04's maker-checker resolution — the
// approving principal must differ from whoever raised the exception,
// enforced at the store layer (domain.ErrMappingExceptionSelfApproval),
// not just checked here. Authorized as a platform-level action (empty
// legal_entity_id) rather than per-legal-entity: mapping exceptions are an
// internal control activity typically worked by a shared ops/finance
// team across entities, not a legal-entity-scoped business operation.

func (h *BNK0304Handler) ApproveMappingException(w http.ResponseWriter, r *http.Request) {
	var req domain.ApproveMappingExceptionParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	req.ExceptionID = chi.URLParam(r, "id")
	req.TenantID = middleware.GetTenantID(r.Context())
	req.ApproverPrincipalID = principalID
	req.CorrelationID = r.Header.Get("X-Correlation-ID")
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", BANKING_MAPPING_EXCEPTION_APPROVE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	txn, err := h.bnk0304Store.ApproveMappingException(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, txn)
}

// CreateTransactionMapping handles POST /v1/banking/transaction-mappings —
// the operational entry point that populates the dictionary
// NormalizeTransaction resolves categories against. Authorized as a
// platform-level action (empty legal_entity_id), the same posture as
// ApproveMappingException: maintaining this dictionary is a shared
// ops/finance control activity, not a legal-entity-scoped business
// operation.
func (h *BNK0304Handler) CreateTransactionMapping(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateTransactionMappingParams
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.BankCode == "" || req.Category == "" {
		writeError(w, http.StatusBadRequest, "bank_code and category are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	req.TenantID = middleware.GetTenantID(r.Context())
	req.ActorPrincipalID = principalID
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", BANKING_MAPPING_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	m, err := h.bnk0304Store.CreateTransactionMapping(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *BNK0304Handler) GetCanonicalTransaction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	txn, err := h.bnk0304Store.GetCanonicalTransaction(r.Context(), tenantID, id)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, txn)
}

// listTransactionsParams are ListTransactions' accepted query parameters. Any
// other is refused, as on the control-population route: a filter misspelled
// by a caller must not silently widen the result.
var listTransactionsParams = map[string]bool{
	"legal_entity_id": true, "bank_account_id": true, "currency": true,
	"transaction_date_from": true, "transaction_date_to": true, "limit": true, "cursor": true,
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// ListCanonicalTransactions is BNK-04 ListTransactions:
//
//	GET /v1/banking/canonical-transactions?legal_entity_id=…[&bank_account_id=…]
//	    [&currency=…][&transaction_date_from=YYYY-MM-DD][&transaction_date_to=YYYY-MM-DD]
//	    [&limit=…][&cursor=…]
//
// NORMALIZED transactions only, of one legal entity, scoped to the request's
// tenant by row-level security. Dates are inclusive UTC calendar days on
// transaction_date. Authorized as bank.transaction.read on the legal entity —
// a real authorization-svc decision, not just a principal header.
//
// Pagination follows the control-population route: limit defaults to 1000
// and is capped at 5000; next_cursor is present exactly when more rows
// exist, so a full page is never indistinguishable from the end of the
// result.
func (h *BNK0304Handler) ListCanonicalTransactions(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "missing_tenant_scope")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	for k, v := range query {
		if !listTransactionsParams[k] {
			writeError(w, http.StatusBadRequest, "unknown query parameter: "+k)
			return
		}
		if len(v) != 1 {
			writeError(w, http.StatusBadRequest, "query parameter must appear once: "+k)
			return
		}
	}

	q := domain.ListTransactionsQuery{Limit: controlDefaultLimit}
	q.LegalEntityID = query.Get("legal_entity_id")
	if q.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if !idPattern.MatchString(q.LegalEntityID) {
		writeError(w, http.StatusBadRequest, "legal_entity_id is invalid")
		return
	}
	if query.Has("bank_account_id") {
		q.BankAccountID = query.Get("bank_account_id")
		if !idPattern.MatchString(q.BankAccountID) {
			writeError(w, http.StatusBadRequest, "bank_account_id is invalid")
			return
		}
	}
	if query.Has("currency") {
		q.Currency = query.Get("currency")
		if !currencyPattern.MatchString(q.Currency) {
			writeError(w, http.StatusBadRequest, "currency must be a three-letter upper-case ISO 4217 code")
			return
		}
	}
	for _, f := range []struct {
		param string
		into  **time.Time
		next  bool // the exclusive upper bound is the day after
	}{{"transaction_date_from", &q.From, false}, {"transaction_date_to", &q.ToExclusive, true}} {
		if !query.Has(f.param) {
			continue
		}
		raw := query.Get(f.param)
		d, err := time.Parse("2006-01-02", raw)
		if err != nil || len(raw) != 10 {
			writeError(w, http.StatusBadRequest, f.param+" must be YYYY-MM-DD")
			return
		}
		if f.next {
			d = d.AddDate(0, 0, 1)
		}
		*f.into = &d
	}
	if q.From != nil && q.ToExclusive != nil && !q.From.Before(*q.ToExclusive) {
		writeError(w, http.StatusBadRequest, "transaction_date_from must not be after transaction_date_to")
		return
	}
	if query.Has("limit") {
		n, err := strconv.Atoi(query.Get("limit"))
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		q.Limit = n
		if q.Limit > controlMaxLimit {
			q.Limit = controlMaxLimit
		}
	}
	if query.Has("cursor") {
		raw, err := base64.RawURLEncoding.DecodeString(query.Get("cursor"))
		if err != nil || len(raw) == 0 || len(raw) > 128 || !utf8.Valid(raw) {
			writeError(w, http.StatusBadRequest, "cursor is invalid")
			return
		}
		q.AfterTransactionID = string(raw)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, q.LegalEntityID, BANK_TRANSACTION_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.bnk0304Store.ListCanonicalTransactions(r.Context(), q)
	if err != nil {
		h.logger.Error("ListCanonicalTransactions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "failed to list canonical transactions")
		return
	}
	if page.NextCursor != "" {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.NextCursor))
	}
	writeJSON(w, http.StatusOK, page)
}
