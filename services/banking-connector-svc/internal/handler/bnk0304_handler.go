// BNK-03 Statement Ingestion + BNK-04 Transaction Normalization's own
// HTTP surface, added to this service's existing /v1/banking routes.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/events"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

const (
	BANKING_STATEMENT_VALIDATE  = "BANKING_STATEMENT_VALIDATE"
	BANKING_TRANSACTION_NORMALIZE = "BANKING_TRANSACTION_NORMALIZE"
	BANKING_MAPPING_EXCEPTION_APPROVE = "BANKING_MAPPING_EXCEPTION_APPROVE"
	BANKING_MAPPING_MANAGE            = "BANKING_MAPPING_MANAGE"
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
	result, err := h.bnk0304Store.IngestStatement(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.logger.Error("IngestStatement failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to ingest bank statement")
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
		_ = h.publisher.Publish(r.Context(), events.PublishParams{
			EventType: "banking.statement.ingested", AggregateID: result.Statement.StatementID, TenantID: tenantID,
			ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: result,
		})
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
	result, err := h.bnk0304Store.NormalizeTransaction(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	if result.Transaction != nil {
		_ = h.publisher.Publish(r.Context(), events.PublishParams{
			EventType: "banking.transaction.normalized", AggregateID: result.Transaction.TransactionID, TenantID: req.TenantID,
			ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: result.Transaction,
		})
		writeJSON(w, http.StatusCreated, result)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "banking.mapping_exception.raised", AggregateID: result.QuarantinedException.ExceptionID, TenantID: req.TenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: result.QuarantinedException,
	})
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
	txn, err := h.bnk0304Store.ReNormalizeTransaction(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "banking.transaction.renormalized", AggregateID: txn.TransactionID, TenantID: req.TenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: txn,
	})
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
	exc, err := h.bnk0304Store.QuarantineTransaction(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "banking.mapping_exception.raised", AggregateID: exc.ExceptionID, TenantID: req.TenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: exc,
	})
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", BANKING_MAPPING_EXCEPTION_APPROVE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	txn, err := h.bnk0304Store.ApproveMappingException(r.Context(), req)
	if err != nil {
		h.writeTransactionErr(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "banking.mapping_exception.approved", AggregateID: req.ExceptionID, TenantID: req.TenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: txn,
	})
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

