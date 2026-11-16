package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
	"zoiko.io/banking-connector-svc/internal/tokenization"
)

const (
	BANKING_CONNECTION_CREATE = "BANKING_CONNECTION_CREATE"
	BANKING_STATEMENT_INGEST  = "BANKING_STATEMENT_INGEST"

	// TOKEN_DETOKENIZE gates the full, unmasked account number. It is the
	// same grant key-management-svc checks before detokenizing (SEC-T15's
	// "additional scoped approval"), checked here too so that a legacy row —
	// still raw, so never sent to the vault — is not an easier way to the
	// same number. One grant, one meaning: "may see raw bank details".
	TOKEN_DETOKENIZE = "TOKEN_DETOKENIZE"
)

type Handler struct {
	store  store.Store
	authz  *authz.Client
	logger *zap.Logger

	// tokenizer detokenizes a tokenized connection's account number for the
	// privileged read. Needed whenever ANY connection is tokenized — which
	// stays true after TOKENIZATION_ENFORCED is turned back off — so it is
	// set whenever a tokenization service is configured, not only when
	// enforcement is on.
	tokenizer tokenization.Tokenizer
}

func New(st store.Store, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, logger: logger}
}

// WithTokenizer sets the client the privileged account-number read
// detokenizes through.
func (h *Handler) WithTokenizer(t tokenization.Tokenizer) *Handler {
	h.tokenizer = t
	return h
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/banking", func(r chi.Router) {
		r.Post("/connections", h.CreateConnection)
		r.Get("/connections", h.ListConnections)
		r.Get("/connections/{id}", h.GetConnectionByID)
		r.Get("/connections/{id}/account-number", h.GetConnectionAccountNumber)
		r.Post("/statements", h.IngestStatement)
		r.Get("/connections/{id}/statements", h.ListStatements)

		// Postman compatibility aliases
		r.Post("/accounts", h.CreateConnection)
		r.Get("/accounts", h.ListConnections)
		r.Get("/accounts/{id}", h.GetConnectionByID)
		r.Get("/accounts/{id}/account-number", h.GetConnectionAccountNumber)
	})
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
// fewer is masked entirely rather than left in the clear. It is
// domain.MaskAccountNumber, which the store also uses to compute a
// tokenized connection's stored masked form.
func maskDigits(s string) string {
	return domain.MaskAccountNumber(s)
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
//
// A TOKENIZED connection (domain.BankConnection.Tokenized — a row in
// bank_connection_account_tokens) is shown by its stored masked form, which
// was computed from the raw number when the token was written. The stored
// account_number is then a token, and masking it would show the token's own
// last characters as if they were account digits. The masked view never
// calls the vault: the last 4 digits are not worth a detokenization, its
// audit event, or a TOKEN_DETOKENIZE grant for every reader. The full number
// is GetConnectionAccountNumber's job, behind its own grant.
func maskSensitiveFields(conn *domain.BankConnection) {
	if conn == nil {
		return
	}
	if conn.Tokenized() {
		conn.AccountNumber = conn.AccountNumberMasked
		conn.IBAN = conn.AccountNumberMasked
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
		Status:        domain.StatusConnected,
	}

	if err := h.store.CreateConnection(r.Context(), conn, principalID, r.Header.Get("X-Correlation-ID")); err != nil {
		h.logger.Error("failed to create bank connection", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create bank connection")
		return
	}

	populateAliases(conn)
	if conn.Tokenized() {
		// The echo-back rationale above covers the RAW number, which this
		// caller just sent. With tokenization enforced the connection now
		// carries the token in account_number/iban, and a token must never
		// be presented as a bank account number — so a tokenized create
		// answers with the masked form, like every other read.
		maskSensitiveFields(conn)
	}
	writeJSON(w, http.StatusCreated, conn)
}

// accountNumberResponse is the privileged account-number read's body.
type accountNumberResponse struct {
	ConnectionID  string `json:"connection_id"`
	AccountNumber string `json:"account_number"`
	IBAN          string `json:"iban"`
	Tokenized     bool   `json:"tokenized"`
}

// GetConnectionAccountNumber is the explicit privileged path to a
// connection's FULL account number — the counterpart to the masked
// GetConnectionByID, for a caller that genuinely needs the real number.
//
//  1. A principal is required, and must hold TOKEN_DETOKENIZE at the
//     connection's own legal entity (from the row, never the caller).
//  2. A tokenized connection is detokenized through key-management-svc
//     under that same principal, so the vault's own TOKEN_DETOKENIZE check,
//     tenant binding (SEC-T29) and audit event (SEC-INV-15) all apply.
//  3. A legacy raw row returns its stored number after step 1.
//
// Fails closed: a vault refusal is 403, an unreachable or unconfigured vault
// is 503. The token is never returned in place of the number.
func (h *Handler) GetConnectionAccountNumber(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	conn, err := h.store.GetConnectionByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "bank connection not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get bank connection")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, conn.LegalEntityID, TOKEN_DETOKENIZE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	number := conn.AccountNumber
	if conn.Tokenized() {
		if h.tokenizer == nil {
			h.logger.Error("tokenized connection read with no tokenization service configured",
				zap.String("connection_id", conn.ConnectionID))
			writeError(w, http.StatusServiceUnavailable, "tokenization service not configured")
			return
		}
		number, err = h.tokenizer.Detokenize(r.Context(), tokenization.Scope{
			TenantID:      middleware.GetTenantID(r.Context()),
			PrincipalID:   principalID,
			LegalEntityID: conn.LegalEntityID,
			CorrelationID: r.Header.Get("X-Correlation-ID"),
		}, conn.AccountNumberToken)
		if err != nil {
			if errors.Is(err, tokenization.ErrDetokenizationDenied) {
				writeError(w, http.StatusForbidden, "detokenization denied")
				return
			}
			h.logger.Error("detokenize account number failed", zap.String("connection_id", conn.ConnectionID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "tokenization service unavailable")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, accountNumberResponse{
		ConnectionID: conn.ConnectionID, AccountNumber: number, IBAN: number, Tokenized: conn.Tokenized(),
	})
}

func (h *Handler) GetConnectionByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	conn, err := h.store.GetConnectionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "bank connection not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get bank connection")
		return
	}
	populateAliases(conn)
	maskSensitiveFields(conn)
	writeJSON(w, http.StatusOK, conn)
}

func (h *Handler) ListConnections(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
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
	connectionID := chi.URLParam(r, "id")
	stmts, err := h.store.ListStatements(r.Context(), connectionID)
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
