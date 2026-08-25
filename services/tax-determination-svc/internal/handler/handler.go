package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-determination-svc/internal/authz"
	"zoiko.io/tax-determination-svc/internal/domain"
	"zoiko.io/tax-determination-svc/internal/events"
	"zoiko.io/tax-determination-svc/internal/middleware"
	"zoiko.io/tax-determination-svc/internal/registry"
	"zoiko.io/tax-determination-svc/internal/rules"
	"zoiko.io/tax-determination-svc/internal/store"
)

// snapshotTaxRule computes a content-addressed reference over the actual
// rule fields tax-rules-svc returned, pinning a determination to the exact
// rule content applied — independent of later edits to the mutable rule row
// rule_id points at. Returns nil for the zero-tax fallback (tax-rules-svc
// unreachable): there is no real rule content to snapshot, and fabricating
// one would misrepresent a fallback as a governed rule application.
func snapshotTaxRule(rule *rules.TaxRuleDTO) *string {
	// "trule-fallback" is this handler's own (currently unreachable) local
	// fallback; "trule-default-fallback"/"trule-default-zero" are
	// rules.Client's actual fallback sentinels — a transport error or an
	// empty rule set from tax-rules-svc, respectively. None represent a
	// real, governed rule application.
	if rule == nil || rule.RuleID == "trule-fallback" ||
		rule.RuleID == "trule-default-fallback" || rule.RuleID == "trule-default-zero" {
		return nil
	}
	canonical := fmt.Sprintf("%s|%s|%s|%.6f|%.6f|%s",
		rule.RuleID, rule.RuleCode, rule.Category, rule.TaxRatePercentage, rule.StandardDeductions, rule.Status)
	sum := sha256.Sum256([]byte(canonical))
	hash := hex.EncodeToString(sum[:])
	return &hash
}

// Action constants for authorization-svc calls, shaped <RESOURCE>_<VERB>.
const (
	ActionTaxDeterminationCreate   = "TAX_DETERMINATION_CREATE"
	ActionTaxDeterminationOverride = "TAX_DETERMINATION_OVERRIDE"
)

// AuthzChecker is the subset of authz.Client's contract the handler depends
// on. It is an interface (rather than *authz.Client directly) so tests can
// substitute a stub without exercising real HTTP calls.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

var _ AuthzChecker = (*authz.Client)(nil)

// JurisdictionValidator probes jurisdiction-rules-svc for the jurisdiction
// references TAX-03 requires. An interface rather than the concrete client so
// the handler's tests can exercise the unknown and unreachable branches, which
// are the two that decide whether a determination is refused or fails closed.
type JurisdictionValidator interface {
	Validate(ctx context.Context, correlationID, jurisdictionID string) error
}

// RegistrationResolver reads TAX-03's server-resolved "Registrations" input —
// whether the seller holds a tax registration in the place of supply.
type RegistrationResolver interface {
	ResolveSellerRegistration(ctx context.Context, tenantID, legalEntityID, jurisdictionID, supplyDate string) (*registry.Registration, error)
}

type Handler struct {
	store        store.Store
	publisher    events.Publisher
	authz        AuthzChecker
	rulesClient  *rules.Client
	jurisdiction JurisdictionValidator
	registry     RegistrationResolver
	logger       *zap.Logger
}

func New(
	st store.Store,
	pub events.Publisher,
	az AuthzChecker,
	rc *rules.Client,
	jv JurisdictionValidator,
	rr RegistrationResolver,
	logger *zap.Logger,
) *Handler {
	return &Handler{
		store: st, publisher: pub, authz: az, rulesClient: rc,
		jurisdiction: jv, registry: rr, logger: logger,
	}
}

// authorize extracts the caller's principal from the X-Principal-Id header
// and checks the action against authorization-svc before any mutation is
// performed. It writes the appropriate error response itself when the
// caller should not proceed.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, legalEntityID, actionType string) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return "", false
	}
	return principalID, true
}

// optional turns an empty string into a nil pointer.
//
// The distinction is not cosmetic: a nil ship_from means "this supply has no
// movement", and an empty string would claim the caller named a jurisdiction
// and it was blank. On a record used to defend a tax position, absent and empty
// are different assertions.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// missingTAX03Input names the first required §9.J input the request omits, or
// "" when all are present. One field per refusal, named, so a caller adopting
// the contract can fix them in sequence.
func missingTAX03Input(req domain.DetermineTaxRequest) string {
	switch {
	case req.SellerPartyID == "":
		return "seller_party_id is required — a determination has to say who is supplying"
	case req.BuyerPartyID == "":
		return "buyer_party_id is required — who is being supplied decides the treatment"
	case req.SupplyJurisdictionID == "":
		return "supply_jurisdiction_id is required — the place of supply is the jurisdiction whose rules govern this transaction"
	case req.SupplyDate == "":
		return "supply_date is required — the tax point decides which rule version applies"
	case req.ProductClassification == "":
		return "product_classification is required"
	case req.SupplyKind == "":
		return "supply_kind is required — one of GOODS, SERVICES, DIGITAL_SERVICES"
	case req.SupplyType == "":
		return "supply_type is required — one of B2B, B2C, B2G"
	case req.Currency == "":
		return "currency is required"
	default:
		return ""
	}
}

// invalidTAX03Input names the first §9.J input that is present but wrong.
//
// Separate from missingTAX03Input because "you did not send supply_kind" and
// "GOOD is not a supply kind" are different mistakes, and only the second can
// be fixed by a caller that is told which value was rejected.
func invalidTAX03Input(req domain.DetermineTaxRequest) string {
	if !domain.ValidSupplyKind(req.SupplyKind) {
		return domain.ErrInvalidSupplyKind.Error()
	}
	if !domain.ValidSupplyType(req.SupplyType) {
		return domain.ErrInvalidSupplyType.Error()
	}
	if !domain.ValidCurrencyCode(req.Currency) {
		return domain.ErrInvalidCurrency.Error()
	}
	if _, err := time.Parse("2006-01-02", req.SupplyDate); err != nil {
		return domain.ErrInvalidSupplyDate.Error()
	}
	// The fact that decides reverse charge. A business buyer carrying no
	// registration in the place of supply is a B2C supply in substance.
	if req.SupplyType == domain.SupplyTypeB2B && req.BuyerTaxRegistrationID == "" {
		return domain.ErrB2BNeedsBuyerRegistration.Error()
	}
	// INV-10: an exemption is the one number here that reduces tax without a
	// rule having said so, and it cannot stand on an amount alone.
	if req.ExemptAmount > 0 && req.ExemptionReason == "" {
		return domain.ErrExemptionNeedsReason.Error()
	}
	if req.ExemptAmount > req.GrossAmount {
		return "exempt_amount cannot exceed gross_amount"
	}
	return ""
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/tax-determinations", func(r chi.Router) {
		r.Post("/", h.DetermineTax)
		r.Get("/", h.ListDeterminations)
		r.Get("/{id}", h.GetDetermination)
		r.Post("/{id}/override", h.OverrideDetermination)
	})
}

func (h *Handler) DetermineTax(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.DetermineTaxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TransactionID == "" || req.JurisdictionID == "" || req.TaxCategory == "" || req.GrossAmount <= 0 {
		writeError(w, http.StatusBadRequest, "transaction_id, jurisdiction_id, tax_category, and positive gross_amount are required")
		return
	}
	// TAX-03 required business/source inputs, checked before authorization so a
	// malformed request is refused as malformed rather than as denied.
	if detail := missingTAX03Input(req); detail != "" {
		writeError(w, http.StatusBadRequest, detail)
		return
	}
	if detail := invalidTAX03Input(req); detail != "" {
		writeError(w, http.StatusBadRequest, detail)
		return
	}

	principalID, ok := h.authorize(w, r, req.LegalEntityID, ActionTaxDeterminationCreate)
	if !ok {
		return
	}

	// Every jurisdiction this determination names must be one
	// jurisdiction-rules-svc recognises. Unknown is the caller's mistake;
	// unreachable is "cannot answer" and fails the determination closed rather
	// than pinning a tax position to a jurisdiction nobody verified.
	correlationID := r.Header.Get("X-Correlation-ID")
	for field, id := range map[string]string{
		"supply_jurisdiction_id":    req.SupplyJurisdictionID,
		"jurisdiction_id":           req.JurisdictionID,
		"ship_from_jurisdiction_id": req.ShipFromJurisdictionID,
		"ship_to_jurisdiction_id":   req.ShipToJurisdictionID,
	} {
		if err := h.jurisdiction.Validate(r.Context(), correlationID, id); err != nil {
			if errors.Is(err, domain.ErrJurisdictionUnknown) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s: %s", field, err.Error()))
				return
			}
			h.logger.Warn("jurisdiction validation unavailable",
				zap.String("field", field), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, domain.ErrJurisdictionUnverifiable.Error())
			return
		}
	}

	// TAX-03 server-resolved input: does the seller hold a tax registration in
	// the place of supply, as at the supply date? Not the caller's to assert —
	// it is what decides whether tax is charged at all.
	//
	// A nil registration is an answer (the seller is not registered there) and
	// is recorded as such. Being unable to ask is not, and fails closed.
	sellerReg, err := h.registry.ResolveSellerRegistration(
		r.Context(), tenantID, req.LegalEntityID, req.SupplyJurisdictionID, req.SupplyDate)
	if err != nil {
		h.logger.Warn("seller registration could not be resolved", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrRegistryUnavailable.Error())
		return
	}

	// Fetch dynamic tax rule from tax-rules-svc
	rule, err := h.rulesClient.FetchActiveRule(r.Context(), tenantID, req.JurisdictionID, req.TaxCategory)
	if err != nil {
		h.logger.Warn("failed to fetch tax rule, using zero tax fallback", zap.Error(err))
		rule = &rules.TaxRuleDTO{
			RuleID:            "trule-fallback",
			TaxRatePercentage: 0,
		}
	}

	taxableAmount := req.GrossAmount - req.ExemptAmount
	if taxableAmount < 0 {
		taxableAmount = 0
	}
	calculatedTax := taxableAmount * (rule.TaxRatePercentage / 100.0)

	det := &domain.TaxDetermination{
		TenantID:            tenantID,
		TransactionID:       req.TransactionID,
		SourceModule:        req.SourceModule,
		LegalEntityID:       req.LegalEntityID,
		JurisdictionID:      req.JurisdictionID,
		RuleID:              rule.RuleID,
		TaxLogicSnapshotID:  snapshotTaxRule(rule),
		TaxCategory:         req.TaxCategory,
		GrossAmount:         req.GrossAmount,
		TaxableAmount:       taxableAmount,
		TaxRatePercentage:   rule.TaxRatePercentage,
		CalculatedTaxAmount: calculatedTax,
		ExemptAmount:        req.ExemptAmount,
		Currency:            req.Currency,
		Status:              domain.StatusCalculated,
		EffectiveFrom:       req.EffectiveFrom,
		EvaluatedBy:         req.EvaluatedBy,

		// TAX-03 inputs, preserved so the determination can be reconstructed
		// from the facts it was given (§9.J retrieval, Appendix B).
		SellerPartyID:         req.SellerPartyID,
		BuyerPartyID:          req.BuyerPartyID,
		SellerEstablishmentID: optional(req.SellerEstablishmentID),
		BuyerEstablishmentID:  optional(req.BuyerEstablishmentID),

		ShipFromJurisdictionID: optional(req.ShipFromJurisdictionID),
		ShipToJurisdictionID:   optional(req.ShipToJurisdictionID),

		SupplyJurisdictionID: req.SupplyJurisdictionID,
		SupplyDate:           optional(req.SupplyDate),
		// No pack carries place-of-supply rules, so this engine did not derive
		// the place of supply — the caller stated it. Recorded rather than left
		// implicit. See domain.PlaceOfSupplyBasis.
		PlaceOfSupplyBasis: domain.PlaceOfSupplyCallerAsserted,

		ProductClassification: req.ProductClassification,
		SupplyKind:            req.SupplyKind,
		SupplyType:            req.SupplyType,

		BuyerTaxRegistrationID: optional(req.BuyerTaxRegistrationID),

		ExemptionReason:         optional(req.ExemptionReason),
		ExemptionCertificateRef: optional(req.ExemptionCertificateRef),
	}

	if sellerReg != nil {
		det.SellerRegistrationID = &sellerReg.BundleID
		status := sellerReg.Status
		det.SellerRegistrationStatus = &status
	}

	if err := h.store.CreateDetermination(r.Context(), det); err != nil {
		h.logger.Error("create tax determination failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to record tax determination")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "tax_determination.calculated", DeterminationID: det.DeterminationID, TenantID: tenantID,
		LegalEntityID: det.LegalEntityID, Jurisdiction: det.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: det,
	})
	writeJSON(w, http.StatusCreated, det)
}

func (h *Handler) GetDetermination(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	det, err := h.store.GetDetermination(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxDeterminationNotFound) {
			writeError(w, http.StatusNotFound, "tax determination not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get tax determination")
		return
	}
	writeJSON(w, http.StatusOK, det)
}

func (h *Handler) ListDeterminations(w http.ResponseWriter, r *http.Request) {
	transactionID := r.URL.Query().Get("transaction_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	status := r.URL.Query().Get("status")
	determinations, err := h.store.ListDeterminations(r.Context(), transactionID, jurisdictionID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tax determinations")
		return
	}
	if determinations == nil {
		determinations = []domain.TaxDetermination{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"determinations": determinations, "total": len(determinations)})
}

func (h *Handler) OverrideDetermination(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.OverrideTaxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required for tax override")
		return
	}

	existing, err := h.store.GetDetermination(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxDeterminationNotFound) {
			writeError(w, http.StatusNotFound, "tax determination not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get tax determination")
		return
	}

	principalID, ok := h.authorize(w, r, existing.LegalEntityID, ActionTaxDeterminationOverride)
	if !ok {
		return
	}

	det, err := h.store.OverrideDetermination(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrTaxDeterminationNotFound):
			writeError(w, http.StatusNotFound, "tax determination not found")
		case errors.Is(err, domain.ErrAlreadyOverridden):
			writeError(w, http.StatusConflict, "tax determination is already overridden")
		default:
			writeError(w, http.StatusInternalServerError, "failed to override tax determination")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "tax_determination.overridden", DeterminationID: id, TenantID: tenantID,
		LegalEntityID: det.LegalEntityID, Jurisdiction: det.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: det,
	})
	writeJSON(w, http.StatusOK, det)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
