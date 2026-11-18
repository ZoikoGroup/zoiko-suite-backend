package handler

// HTTP surface for ORG-08 addresses.
//
//	POST /v1/addresses                              CreateAddress (201 created, 200 replay)
//	GET  /v1/addresses?q=&country=&postal_code=&type=  SearchAddresses
//	GET  /v1/addresses/{id}?as_of=&history=true     GetAddress
//	POST /v1/addresses/{id}/versions                AmendAddress (201 new version, 200 nothing changed)
//	POST /v1/addresses/{id}:verify                  VerifyAddress
//	POST /v1/parties/{id}/addresses                 LinkAddressToParty (201, 200 already linked)
//	GET  /v1/parties/{id}/addresses?as_of=&include_ended=true   a party's addresses, current or historical
//
// Mounted only when the service satisfies AddressService, which *registry.Service does.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// AddressService is the ORG-08 address half of the service contract.
type AddressService interface {
	CreateAddress(ctx context.Context, req domain.CreateAddressRequest) (*registry.AddressOutcome, error)
	AmendAddress(ctx context.Context, addressID string, req domain.AmendAddressRequest) (*registry.AddressOutcome, error)
	VerifyAddress(ctx context.Context, addressID string, req domain.VerifyAddressRequest) (*registry.VerifyOutcome, error)
	LinkAddressToParty(ctx context.Context, partyID string, req domain.LinkAddressRequest) (*registry.AddressLinkOutcome, error)
	GetAddress(ctx context.Context, addressID string, asOf time.Time, history bool) (*domain.AddressView, error)
	SearchAddresses(ctx context.Context, q domain.AddressSearchQuery) ([]domain.AddressSummary, string, error)
	ListPartyAddresses(ctx context.Context, partyID string, asOf time.Time, includeEnded bool) ([]domain.PartyAddress, error)
}

func registerAddressRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(AddressService); !ok {
		return
	}
	r.Post("/addresses", h.CreateAddress)
	r.Get("/addresses", h.SearchAddresses)
	r.Get("/addresses/{addressID}", h.GetAddress)
	r.Post("/addresses/{addressID}/versions", h.AmendAddress)
	r.Post("/addresses/{addressID}:verify", h.VerifyAddress)
	r.Post("/parties/{partyID}/addresses", h.LinkAddressToParty)
	r.Get("/parties/{partyID}/addresses", h.ListPartyAddresses)
}

func (h *Handler) addresses() AddressService {
	as, _ := h.svc.(AddressService)
	return as
}

// CreateAddress records an address.
func (h *Handler) CreateAddress(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAddressRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.addresses().CreateAddress(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if out.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// AmendAddress adds a version.
func (h *Handler) AmendAddress(w http.ResponseWriter, r *http.Request) {
	var req domain.AmendAddressRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.addresses().AmendAddress(r.Context(), chi.URLParam(r, "addressID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if out.NoOp {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// VerifyAddress records a verification observation.
func (h *Handler) VerifyAddress(w http.ResponseWriter, r *http.Request) {
	var req domain.VerifyAddressRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.addresses().VerifyAddress(r.Context(), chi.URLParam(r, "addressID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// LinkAddressToParty uses an address for a party.
func (h *Handler) LinkAddressToParty(w http.ResponseWriter, r *http.Request) {
	var req domain.LinkAddressRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.addresses().LinkAddressToParty(r.Context(), chi.URLParam(r, "partyID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if out.NoOp {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// GetAddress returns an address with the version in force at as_of.
func (h *Handler) GetAddress(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	v, err := h.addresses().GetAddress(r.Context(), chi.URLParam(r, "addressID"), asOf, r.URL.Query().Get("history") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// SearchAddresses searches the normalized comparison form.
func (h *Handler) SearchAddresses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeErrJSON(w, r, http.StatusBadRequest, "limit must be an integer from 1 to 200", correlationID(r))
			return
		}
		limit = n
	}
	out, next, err := h.addresses().SearchAddresses(r.Context(), domain.AddressSearchQuery{
		Text: q.Get("q"), Country: q.Get("country"), PostalCode: q.Get("postal_code"),
		Type: domain.AddressType(q.Get("type")), Limit: limit, After: q.Get("cursor"),
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}

// ListPartyAddresses lists a party's addresses.
func (h *Handler) ListPartyAddresses(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.addresses().ListPartyAddresses(r.Context(), chi.URLParam(r, "partyID"), asOf, r.URL.Query().Get("include_ended") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// The production service offers addresses; if it ever stops, the routes would silently vanish.
var _ AddressService = (*registry.Service)(nil)
