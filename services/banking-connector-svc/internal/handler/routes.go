package handler

import (
	"github.com/go-chi/chi/v5"

	"zoiko.io/banking-connector-svc/internal/clients"
	"zoiko.io/banking-connector-svc/internal/store"
)

// AllStores is the full persistence surface the HTTP API needs; *store.PgStore
// satisfies it.
type AllStores interface {
	store.BNK02Store
	store.BNK0304Store
	store.ControlPopulationStore
}

// RegisterAll registers every business route of the service on r, in the
// order cmd/server/main.go uses. It exists so routes_contract_test.go checks
// the exact router production serves, not a hand-copied approximation.
func RegisterAll(r chi.Router, h *Handler, st AllStores, bankAccts clients.BankAccountClient, vault clients.VaultClient) {
	RegisterRoutes(r, h)
	RegisterBNK02Routes(r, h, st, bankAccts, vault)
	RegisterBNK0304Routes(r, h, st)
	RegisterControlPopulationRoutes(r, h, st)
}
