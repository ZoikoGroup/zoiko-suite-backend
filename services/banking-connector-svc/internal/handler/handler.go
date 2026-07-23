package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/events"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/banking", func(r chi.Router) {
		r.Post("/connections", h.CreateConnection)
		r.Get("/connections", h.ListConnections)
		r.Get("/connections/{id}", h.GetConnectionByID)
		r.Post("/statements", h.IngestStatement)
		r.Get("/connections/{id}/statements", h.ListStatements)

		// Postman compatibility aliases
		r.Post("/accounts", h.CreateConnection)
		r.Get("/accounts", h.ListConnections)
		r.Get("/accounts/{id}", h.GetConnectionByID)
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

	if err := h.store.CreateConnection(r.Context(), conn); err != nil {
		h.logger.Error("failed to create bank connection", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create bank connection")
		return
	}

	populateAliases(conn)
	_ = h.publisher.Publish(r.Context(), "banking.connection.created", conn.ConnectionID, tenantID, conn)
	writeJSON(w, http.StatusCreated, conn)
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

	if _, err := h.store.GetConnectionByID(r.Context(), req.ConnectionID); err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "bank connection not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify bank connection")
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

	if err := h.store.RecordStatement(r.Context(), stmt); err != nil {
		h.logger.Error("failed to ingest statement", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to ingest bank statement")
		return
	}

	_ = h.publisher.Publish(r.Context(), "banking.statement.ingested", stmt.StatementID, tenantID, stmt)
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
