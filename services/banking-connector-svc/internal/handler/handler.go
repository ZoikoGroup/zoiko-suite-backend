package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

const (
	BANKING_CONNECTION_CREATE = "BANKING_CONNECTION_CREATE"
	BANKING_STATEMENT_INGEST  = "BANKING_STATEMENT_INGEST"

	// Read actions. A connection, its statements and its canonical
	// transactions are authorized for the connection's legal entity; a list
	// for the requested legal entity, else the caller's tenant.
	BANKING_CONNECTION_READ  = "BANKING_CONNECTION_READ"
	BANKING_STATEMENT_READ   = "BANKING_STATEMENT_READ"
	BANKING_TRANSACTION_READ = "BANKING_TRANSACTION_READ"
)

type Handler struct {
	store  store.Store
	authz  *authz.Client
	logger *zap.Logger
}

func New(st store.Store, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	// Full paths on the shared router, not r.Route("/v1/banking", ...): the
	// BNK-02/03/04 registrations add routes under the same prefix, and a
	// mounted subrouter here plus their routes there made one of the two
	// unreachable (route shadowing -> 404). routes_contract_test.go pins it.
	r.Post("/v1/banking/connections", h.CreateConnection)
	r.Get("/v1/banking/connections", h.ListConnections)
	r.Get("/v1/banking/connections/{id}", h.GetConnectionByID)
	r.Post("/v1/banking/statements", h.IngestStatement)
	r.Get("/v1/banking/connections/{id}/statements", h.ListStatements)

	// Postman compatibility aliases
	r.Post("/v1/banking/accounts", h.CreateConnection)
	r.Get("/v1/banking/accounts", h.ListConnections)
	r.Get("/v1/banking/accounts/{id}", h.GetConnectionByID)
}

func populateAliases(conn *domain.BankConnection) {
	if conn == nil {
		return
	}
	conn.AccountID = conn.ConnectionID
	if conn.SwiftBIC == "" {
		conn.SwiftBIC = conn.BIC
	}
	if conn.IBAN == "" {
		conn.IBAN = conn.AccountNumber
	}
}

// maskDigits keeps only the last 4 characters of a bank identifier visible,
// replacing everything before them with "*". A value of 4 characters or
// fewer is masked entirely rather than left in the clear.
func maskDigits(s string) string {
	if s == "" {
		return s
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}

// maskSensitiveFields redacts AccountNumber and IBAN before a connection is
// returned from a read path (get-by-id, list) that a principal other than
// the one who submitted the number may reach. The master register's
// ZS-SVC-F-001-derived invariant #2 forbids these fields appearing in the
// clear in list/read APIs. CreateConnection's own response is left
// unmasked deliberately: that response echoes the value back to the same
// caller who just submitted it, so masking it there would create no
// security benefit while breaking any client that expects to see what it
// sent. The underlying stored value is never touched — only the copy
// serialized onto the wire here.
func maskSensitiveFields(conn *domain.BankConnection) {
	if conn == nil {
		return
	}
	conn.AccountNumber = maskDigits(conn.AccountNumber)
	conn.IBAN = maskDigits(conn.IBAN)
}

func (h *Handler) CreateConnection(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	bic := req.BIC
	if bic == "" {
		bic = req.SwiftBIC
	}
	accNum := req.AccountNumber
	if accNum == "" {
		accNum = req.IBAN
	}

	if req.LegalEntityID == "" || req.BankName == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and bank_name are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, BANKING_CONNECTION_CREATE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// No bank has been contacted and no consent exists: the connection is a
	// request, not a working link. It starts REQUESTED, the BNK-02 initial
	// state, and becomes ACTIVE only through the consent lifecycle
	// (complete-authorization -> activate). It used to be stored CONNECTED,
	// asserting a connection that was never made.
	conn := &domain.BankConnection{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		BankName:      req.BankName,
		BIC:           bic,
		SwiftBIC:      bic,
		AccountNumber: accNum,
		IBAN:          accNum,
		Currency:      req.Currency,
		AccountType:   req.AccountType,
		Status:        domain.ConnStatusRequested,
	}

	if err := h.store.CreateConnection(r.Context(), conn, principalID, r.Header.Get("X-Correlation-ID")); err != nil {
		h.logger.Error("failed to create bank connection", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create bank connection")
		return
	}

	populateAliases(conn)
	writeJSON(w, http.StatusCreated, conn)
}

// loadConnectionForRead requires a principal, resolves {id} within the
// caller's tenant (404 for a missing or foreign connection, before any
// authorization is asked) and authorizes action for the connection's legal
// entity.
func (h *Handler) loadConnectionForRead(w http.ResponseWriter, r *http.Request, action string) (*domain.BankConnection, bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	conn, err := h.store.GetConnectionByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "bank connection not found")
			return nil, false
		}
		h.logger.Error("failed to get bank connection", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to get bank connection")
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return nil, false
	}
	return conn, true
}

func (h *Handler) GetConnectionByID(w http.ResponseWriter, r *http.Request) {
	conn, ok := h.loadConnectionForRead(w, r, BANKING_CONNECTION_READ)
	if !ok {
		return
	}
	populateAliases(conn)
	maskSensitiveFields(conn)
	writeJSON(w, http.StatusOK, conn)
}

func (h *Handler) ListConnections(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	scope := legalEntityID
	if scope == "" {
		scope = middleware.GetTenantID(r.Context())
	}
	if !h.authorizeRead(w, r, scope, BANKING_CONNECTION_READ) {
		return
	}
	conns, err := h.store.ListConnections(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list bank connections")
		return
	}
	for i := range conns {
		populateAliases(&conns[i])
		maskSensitiveFields(&conns[i])
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"connections": conns,
		"accounts":    conns,
		"total":       len(conns),
	})
}

func (h *Handler) IngestStatement(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.IngestStatementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ConnectionID == "" {
		writeError(w, http.StatusBadRequest, "connection_id is required")
		return
	}

	conn, err := h.store.GetConnectionByID(r.Context(), req.ConnectionID)
	if err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "bank connection not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify bank connection")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, BANKING_STATEMENT_INGEST); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	stmt := &domain.BankStatement{
		ConnectionID:    req.ConnectionID,
		TenantID:        tenantID,
		StatementFormat: req.StatementFormat,
		StatementDate:   req.StatementDate,
		OpeningBalance:  req.OpeningBalance,
		ClosingBalance:  req.ClosingBalance,
	}

	if err := h.store.RecordStatement(r.Context(), stmt, conn.LegalEntityID, principalID, r.Header.Get("X-Correlation-ID")); err != nil {
		h.logger.Error("failed to ingest statement", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to ingest bank statement")
		return
	}

	writeJSON(w, http.StatusCreated, stmt)
}

func (h *Handler) ListStatements(w http.ResponseWriter, r *http.Request) {
	conn, ok := h.loadConnectionForRead(w, r, BANKING_STATEMENT_READ)
	if !ok {
		return
	}
	stmts, err := h.store.ListStatements(r.Context(), conn.ConnectionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list statements")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"statements": stmts, "total": len(stmts)})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification. A request with no resolved
// principal never passed identity verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// authorizeRead authorizes a list read scoped to legalEntityID
// (purchase-request-svc pattern). A request with no principal is served only
// when it declares itself an internal service call (X-Source-Channel: system;
// treasury-svc's BNK-01 connection-option lookup is such a caller); it is
// still tenant-scoped by RLS. Anything else needs a principal holding action:
// 401 without one, 403 on denial, 503 when authorization-svc gives no
// decision.
func (h *Handler) authorizeRead(w http.ResponseWriter, r *http.Request, legalEntityID, action string) bool {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		if strings.EqualFold(r.Header.Get("X-Source-Channel"), "system") {
			return true
		}
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// writeAuthzErr maps an authz.CheckAllowed error to the appropriate HTTP
// response. Denial is 403; any other error (including authorization-svc
// being unreachable) is 503 — fail closed, never allow silently.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}
