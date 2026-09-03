package handler

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/compensation-svc/internal/domain"
	svcmiddleware "zoiko.io/compensation-svc/internal/middleware"
)

// ── POST /v1/compensation/components ─────────────────────────────────────────

func (h *Handler) CreateComponent(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateComponentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Name == "" || req.Code == "" || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, name, code, currency are required")
		return
	}

	if req.ComponentType != domain.ComponentEarning && req.ComponentType != domain.ComponentDeduction {
		writeError(w, http.StatusBadRequest, "invalid_component_type", string(domain.ErrInvalidComponentType))
		return
	}

	// A negative default would turn an earning into a deduction without saying so.
	if req.DefaultAmount != nil && *req.DefaultAmount < 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", "default_amount must not be negative")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCompCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Taxable unless the caller says otherwise: assuming a new component is
	// tax-free would under-report income.
	isTaxable := true
	if req.IsTaxable != nil {
		isTaxable = *req.IsTaxable
	}

	now := time.Now().UTC()
	c := &domain.SalaryComponent{
		ComponentID:   uuid.NewString(),
		TenantID:      svcmiddleware.TenantFromContext(r.Context()),
		LegalEntityID: req.LegalEntityID,
		Name:          req.Name,
		Code:          req.Code,
		ComponentType: req.ComponentType,
		IsTaxable:     isTaxable,
		DefaultAmount: req.DefaultAmount,
		Currency:      req.Currency,
		Description:   req.Description,
		Status:        "ACTIVE",
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := h.store.CreateComponent(r.Context(), c); errors.Is(err, domain.ErrComponentCodeExists) {
		writeError(w, http.StatusConflict, "component_code_exists", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to create salary component", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, c)
}

// ── GET /v1/compensation/components ──────────────────────────────────────────

func (h *Handler) ListComponents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	componentType := q.Get("component_type")
	includeInactive := q.Get("include_inactive") == "true"

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}

	if componentType != "" &&
		componentType != domain.ComponentEarning &&
		componentType != domain.ComponentDeduction {
		writeError(w, http.StatusBadRequest, "invalid_component_type", string(domain.ErrInvalidComponentType))
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCompView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListComponents(r.Context(), legalEntityID, componentType, includeInactive)
	if err != nil {
		h.log.Error("failed to list salary components", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.SalaryComponent{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── DELETE /v1/compensation/components/{id} ──────────────────────────────────

func (h *Handler) DeactivateComponent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	component, err := h.store.GetComponent(r.Context(), id)
	if errors.Is(err, domain.ErrComponentNotFound) {
		writeError(w, http.StatusNotFound, "component_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to fetch salary component", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, component.LegalEntityID, actionCompCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.DeactivateComponent(r.Context(), id); errors.Is(err, domain.ErrComponentNotFound) {
		writeError(w, http.StatusNotFound, "component_not_found", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to deactivate salary component", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	component.Status = "INACTIVE"
	component.UpdatedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, component)
}

// ── PUT /v1/compensation/structures/{id}/components ──────────────────────────

func (h *Handler) SetStructureComponents(w http.ResponseWriter, r *http.Request) {
	structureID := chi.URLParam(r, "id")

	var req domain.SetStructureComponentsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	seen := make(map[string]bool, len(req.Components))
	for _, c := range req.Components {
		if c.ComponentID == "" {
			writeError(w, http.StatusBadRequest, "missing_fields", "each component requires component_id")
			return
		}
		if seen[c.ComponentID] {
			writeError(w, http.StatusBadRequest, "duplicate_component", string(domain.ErrDuplicateComponent))
			return
		}
		seen[c.ComponentID] = true

		if c.CalculationMethod != domain.MethodFixed && c.CalculationMethod != domain.MethodPercentOfBase {
			writeError(w, http.StatusBadRequest, "invalid_calculation_method", string(domain.ErrInvalidCalcMethod))
			return
		}
		if c.CalculationValue < 0 ||
			(c.CalculationMethod == domain.MethodPercentOfBase && c.CalculationValue > 100) {
			writeError(w, http.StatusBadRequest, "invalid_calculation_value", string(domain.ErrInvalidCalcValue))
			return
		}
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	structure, err := h.store.GetStructure(r.Context(), structureID)
	if errors.Is(err, domain.ErrStructureNotFound) {
		writeError(w, http.StatusNotFound, "structure_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to fetch structure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, structure.LegalEntityID, actionCompCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// A structure must not be composed from another entity's components: the
	// foreign key would accept it, but the resulting payslip would cross a
	// legal-entity boundary.
	for _, c := range req.Components {
		component, err := h.store.GetComponent(r.Context(), c.ComponentID)
		if errors.Is(err, domain.ErrComponentNotFound) {
			writeError(w, http.StatusNotFound, "component_not_found", err.Error())
			return
		}
		if err != nil {
			h.log.Error("failed to verify component", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
		if component.LegalEntityID != structure.LegalEntityID {
			writeError(w, http.StatusBadRequest, "component_entity_mismatch", string(domain.ErrComponentEntityMismatch))
			return
		}
	}

	if err := h.store.SetStructureComponents(r.Context(), structureID, req.Components); errors.Is(err, domain.ErrComponentNotFound) {
		writeError(w, http.StatusNotFound, "component_not_found", err.Error())
		return
	} else if errors.Is(err, domain.ErrDuplicateComponent) {
		writeError(w, http.StatusBadRequest, "duplicate_component", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to set structure components", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	list, err := h.store.ListStructureComponents(r.Context(), structureID)
	if err != nil {
		h.log.Error("failed to read back structure components", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.StructureComponent{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/compensation/structures/{id}/components ──────────────────────────

func (h *Handler) ListStructureComponents(w http.ResponseWriter, r *http.Request) {
	structureID := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	structure, err := h.store.GetStructure(r.Context(), structureID)
	if errors.Is(err, domain.ErrStructureNotFound) {
		writeError(w, http.StatusNotFound, "structure_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to fetch structure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, structure.LegalEntityID, actionCompView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListStructureComponents(r.Context(), structureID)
	if err != nil {
		h.log.Error("failed to list structure components", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.StructureComponent{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/compensation/structures/{id}/breakdown?base_amount=N ─────────────
//
// Resolves a structure against a base amount and returns the full payslip
// arithmetic. This is what payroll-run-svc needs in order to produce a payslip
// it can explain line by line.

func (h *Handler) GetStructureBreakdown(w http.ResponseWriter, r *http.Request) {
	structureID := chi.URLParam(r, "id")

	raw := r.URL.Query().Get("base_amount")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "base_amount is required")
		return
	}
	baseAmount, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_base_amount", "base_amount must be a number")
		return
	}
	if baseAmount < 0 {
		writeError(w, http.StatusBadRequest, "invalid_base_amount", string(domain.ErrNegativeBaseAmount))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	structure, err := h.store.GetStructure(r.Context(), structureID)
	if errors.Is(err, domain.ErrStructureNotFound) {
		writeError(w, http.StatusNotFound, "structure_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to fetch structure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, structure.LegalEntityID, actionCompView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	components, err := h.store.ListStructureComponents(r.Context(), structureID)
	if err != nil {
		h.log.Error("failed to list structure components", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ComputeBreakdown(*structure, components, baseAmount))
}

// ComputeBreakdown resolves a structure's components against a base amount.
//
// Every PERCENT_OF_BASE component is taken against baseAmount, never against a
// running total, so the result does not depend on the order components happen
// to be stored in. Amounts are rounded to two decimal places per line, so the
// totals equal the sum of the lines a reader can see on the payslip rather than
// drifting from them by a fraction of a cent.
func ComputeBreakdown(structure domain.CompensationStructure, components []domain.StructureComponent, baseAmount float64) domain.CompensationBreakdown {
	breakdown := domain.CompensationBreakdown{
		StructureID:   structure.StructureID,
		StructureName: structure.Name,
		Currency:      structure.Currency,
		BaseAmount:    round2(baseAmount),
		Lines:         []domain.BreakdownLine{},
	}

	// The base itself is taxable pay; components add to or subtract from it.
	taxable := breakdown.BaseAmount

	for _, c := range components {
		amount := c.CalculationValue
		if c.CalculationMethod == domain.MethodPercentOfBase {
			amount = baseAmount * c.CalculationValue / 100
		}
		amount = round2(amount)

		breakdown.Lines = append(breakdown.Lines, domain.BreakdownLine{
			ComponentID:       c.ComponentID,
			ComponentCode:     c.ComponentCode,
			ComponentName:     c.ComponentName,
			ComponentType:     c.ComponentType,
			IsTaxable:         c.IsTaxable,
			CalculationMethod: c.CalculationMethod,
			CalculationValue:  c.CalculationValue,
			Amount:            amount,
			Sequence:          c.Sequence,
		})

		switch c.ComponentType {
		case domain.ComponentEarning:
			breakdown.TotalEarnings += amount
			if c.IsTaxable {
				taxable += amount
			}
		case domain.ComponentDeduction:
			breakdown.TotalDeductions += amount
			// A taxable deduction (a salary sacrifice, say) reduces taxable pay.
			if c.IsTaxable {
				taxable -= amount
			}
		}
	}

	breakdown.TotalEarnings = round2(breakdown.TotalEarnings)
	breakdown.TotalDeductions = round2(breakdown.TotalDeductions)
	breakdown.GrossEarnings = round2(breakdown.BaseAmount + breakdown.TotalEarnings)
	breakdown.NetAmount = round2(breakdown.GrossEarnings - breakdown.TotalDeductions)

	// Deductions can exceed taxable pay on paper; reporting negative taxable
	// income would be wrong, so it floors at zero.
	breakdown.TaxableAmount = round2(math.Max(taxable, 0))

	return breakdown
}

// round2 rounds to two decimal places, away from zero at the midpoint.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
