package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

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
	})
}

func (h *Handler) CreateConnection(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.BankName == "" || req.AccountNumber == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, bank_name, and account_number are required")
		return
	}

	c := &domain.BankConnection{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		BankName:      req.BankName,
		BIC:           req.BIC,
		AccountNumber: req.AccountNumber,
		Currency:      req.Currency,
		Status:        domain.StatusConnected,
	}

	if err := h.store.CreateConnection(r.Context(), c); err != nil {
		h.logger.Error("failed to create bank connection", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create bank connection")
		return
	}

	_ = h.publisher.Publish(r.Context(), "bank.connection.created", c.ConnectionID, tenantID, c)
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetConnectionByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, err := h.store.GetConnectionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrConnectionNotFound) {
			writeError(w, http.StatusNotFound, "bank connection not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get bank connection")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListConnections(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	connections, err := h.store.ListConnections(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list bank connections")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"connections": connections,
		"total":       len(connections),
	})
}

func (h *Handler) IngestStatement(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.IngestStatementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ConnectionID == "" || req.StatementFormat == "" {
		writeError(w, http.StatusBadRequest, "connection_id and statement_format are required")
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

	stmtDate := req.StatementDate
	if stmtDate.IsZero() {
		stmtDate = time.Now().UTC()
	}

	stmt := &domain.BankStatement{
		ConnectionID:    req.ConnectionID,
		TenantID:        tenantID,
		StatementFormat: req.StatementFormat,
		StatementDate:   stmtDate,
		OpeningBalance:  req.OpeningBalance,
		ClosingBalance:  req.ClosingBalance,
		TransactionCount: 1,
	}

	if err := h.store.RecordStatement(r.Context(), stmt); err != nil {
		h.logger.Error("failed to record statement", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to ingest bank statement")
		return
	}

	_ = h.publisher.Publish(r.Context(), "bank.statement.ingested", stmt.StatementID, tenantID, stmt)
	writeJSON(w, http.StatusCreated, stmt)
}

func (h *Handler) ListStatements(w http.ResponseWriter, r *http.Request) {
	connectionID := chi.URLParam(r, "id")
	stmts, err := h.store.ListStatements(r.Context(), connectionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list bank statements")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"statements": stmts,
		"total":      len(stmts),
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
