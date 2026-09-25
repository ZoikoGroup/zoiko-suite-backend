// COM-01 Product & Price Book HTTP surface (ZS-SVC-Q-001 §4.1, §10).
package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/store"
)

// authorization-svc action types for the price book. Proposing and approving
// are separate grants (Product/Commercial propose; Finance approves monetary
// terms), and the store additionally refuses an approver who created or
// submitted the version, whatever grants they hold.
const (
	ActionPriceBookPropose = "COMMERCIAL_PRICE_BOOK_PROPOSE"
	ActionPriceBookApprove = "COMMERCIAL_PRICE_BOOK_APPROVE"
	ActionPriceBookPublish = "COMMERCIAL_PRICE_BOOK_PUBLISH"
	ActionPriceBookRetire  = "COMMERCIAL_PRICE_BOOK_RETIRE"
	ActionPriceBookRead    = "COMMERCIAL_PRICE_BOOK_READ"
	ActionCurrencyManage   = "COMMERCIAL_CURRENCY_MANAGE"
)

const maxCommandBody = 1 << 20

var (
	currencyPathPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	queryCodePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	queryMarketPattern  = regexp.MustCompile(`^[A-Z]{2,8}$`)
)

// PriceBookHandler serves COM-01. It is separate from Handler so the legacy
// doc7 catalog routes and their tests are untouched while COM-02 migrates
// subscriptions onto price versions.
type PriceBookHandler struct {
	store  store.PriceBookStore
	authz  AuthzChecker
	logger *zap.Logger
	now    func() time.Time
}

func NewPriceBookHandler(st store.PriceBookStore, az AuthzChecker, logger *zap.Logger) *PriceBookHandler {
	return &PriceBookHandler{store: st, authz: az, logger: logger, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock replaces the server clock. Server time decides every effective
// boundary; the clock is never taken from a request.
func (h *PriceBookHandler) WithClock(now func() time.Time) *PriceBookHandler {
	h.now = now
	return h
}

func RegisterPriceBookRoutes(r chi.Router, h *PriceBookHandler) {
	r.Route("/v1/commercial", func(r chi.Router) {
		r.Get("/currencies", h.ListCurrencies)
		r.Put("/currencies/{code}", h.PutCurrency)

		r.Post("/products", h.CreateProduct)
		r.Get("/products", h.GetPublishedProducts)
		r.Get("/products/{productID}/price-history", h.GetPriceHistory)

		r.Post("/price-versions", h.CreateDraftVersion)
		r.Get("/price-versions:compare", h.CompareVersions)
		r.Get("/price-versions/{id}", h.GetPriceVersion)
		// Custom methods (…/{id}:approve) share the {id} segment with the plain
		// resource route, so they are dispatched from one handler rather than
		// relying on router suffix matching.
		r.Post("/price-versions/{id}", h.PriceVersionAction)
		r.Get("/price-versions/{id}/capabilities", h.GetPlanCapabilities)
		r.Put("/price-versions/{id}/capabilities", h.SetPlanCapabilities)
		r.Put("/price-versions/{id}/commercial-terms", h.SetCommercialTerms)
		r.Put("/price-versions/{id}/components/{key}", h.PutPriceComponent)
		r.Delete("/price-versions/{id}/components/{key}", h.RemovePriceComponent)

		r.Get("/sellable-offers", h.ResolveSellableOffers)
	})
}

// ── Request plumbing ─────────────────────────────────────────────────────────

type command struct {
	principal string
	claim     domain.IdempotencyClaim
	ifMatch   int
}

func (h *PriceBookHandler) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated,
			Detail: "X-Principal-Id is required — the gateway sets it from a verified identity envelope"})
		return "", false
	}
	return p, true
}

func (h *PriceBookHandler) authorize(w http.ResponseWriter, r *http.Request, principal, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, platformScopeID, action); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied,
				Detail: "principal is not granted " + action})
		} else {
			writeProblem(w, r, Problem{Status: http.StatusServiceUnavailable, Code: CodeAuthorizationUnavailable,
				Detail: "authorization service unavailable; the request was refused rather than allowed"})
		}
		return false
	}
	return true
}

// sellerView decides whether a read may see unpublished versions. It fails to
// the public view — on denial and on an authorization outage alike — so a
// read can lose visibility of drafts but never gain it.
func (h *PriceBookHandler) sellerView(r *http.Request) bool {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		return false
	}
	return h.authz.CheckAllowed(r.Context(), p, platformScopeID, ActionPriceBookRead) == nil
}

// readBody decodes a JSON command body strictly: unknown fields are refused,
// so a client cannot slip in a status, a price or an approval the server does
// not accept from it. It also returns the raw bytes for the request hash.
func readBody(w http.ResponseWriter, r *http.Request, dst any, optional bool) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCommandBody))
	if err != nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "request body could not be read or exceeds 1 MiB"})
		return nil, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if optional {
			return raw, true
		}
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "request body is required"})
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "invalid request body: " + err.Error()})
		return nil, false
	}
	if dec.More() {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "request body must be a single JSON object"})
		return nil, false
	}
	return raw, true
}

// newCommand assembles the idempotency claim for a seller-plane command. The
// request hash covers method, path, If-Match and body: reusing a key for any
// different request is refused, not silently replayed.
func (h *PriceBookHandler) newCommand(w http.ResponseWriter, r *http.Request, principal, operation, resourceID string, raw []byte, needsIfMatch bool) (*command, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeIdempotencyKeyRequired,
			Detail: "Idempotency-Key header (at most 255 characters) is required on every command"})
		return nil, false
	}
	cmd := &command{principal: principal}
	ifMatchRaw := strings.TrimSpace(r.Header.Get("If-Match"))
	if needsIfMatch {
		if ifMatchRaw == "" {
			writeProblem(w, r, Problem{Status: http.StatusPreconditionRequired, Code: CodePreconditionRequired,
				Detail: "If-Match with the version's current ETag (row_version) is required"})
			return nil, false
		}
		n, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(ifMatchRaw, "W/"), `"`))
		if err != nil || n < 1 {
			writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
				Detail: `If-Match must be the version's ETag, e.g. "3"`})
			return nil, false
		}
		cmd.ifMatch = n
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\n%s\n%s\n", r.Method, r.URL.Path, ifMatchRaw)
	sum.Write(raw)
	cmd.claim = domain.IdempotencyClaim{
		OwnerScope: domain.SellerScope, PrincipalID: principal, Key: key, Operation: operation,
		RequestSHA256: hex.EncodeToString(sum.Sum(nil)), ResourceID: resourceID,
	}
	return cmd, true
}

func parseID(w http.ResponseWriter, r *http.Request, prefix, raw string) (string, bool) {
	id, err := domain.ParseCommercialID(prefix, raw)
	if err != nil {
		if errors.Is(err, domain.ErrCrossPlaneIdentifier) {
			writeProblem(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: CodeCrossPlaneAccessDenied,
				Detail: "a bare UUID is a tenant-plane identifier; commercial resources are addressed by commercial identifiers"})
		} else {
			writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Detail: err.Error()})
		}
		return "", false
	}
	return id, true
}

func writeVersion(w http.ResponseWriter, status int, v *domain.PriceVersion) {
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(v.RowVersion)))
	writeJSON(w, status, v)
}

// fail maps a store or domain error to its problem response.
func (h *PriceBookHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *domain.ValidationError
	var blocked *domain.PublicationBlockedError
	switch {
	case errors.As(err, &ve):
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: ve.Field, Detail: ve.Error()})
	case errors.As(err, &blocked):
		writeProblem(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: CodePublicationBlocked,
			Detail: "the version cannot move forward until every listed reason is resolved", Reasons: blocked.Reasons})
	case errors.Is(err, domain.ErrCrossPlaneIdentifier):
		writeProblem(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: CodeCrossPlaneAccessDenied, Detail: err.Error()})
	case errors.Is(err, domain.ErrInvalidCommercialID), errors.Is(err, domain.ErrVersionsNotComparable):
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Detail: err.Error()})
	case errors.Is(err, domain.ErrCurrencyNotFound):
		writeProblem(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: CodeInvalidCommercialContext,
			Detail: "currency is not registered in the commercial currency table"})
	case errors.Is(err, domain.ErrProductNotFound), errors.Is(err, domain.ErrPriceVersionNotFound),
		errors.Is(err, domain.ErrPriceComponentNotFound):
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: err.Error()})
	case errors.Is(err, domain.ErrProductCodeTaken):
		writeProblem(w, r, Problem{Status: http.StatusConflict, Code: CodeProductCodeTaken, Detail: err.Error()})
	case errors.Is(err, domain.ErrInFlightVersionExists):
		writeProblem(w, r, Problem{Status: http.StatusConflict, Code: CodePriceVersionInFlight, Detail: err.Error()})
	case errors.Is(err, domain.ErrPriceVersionImmutable):
		writeProblem(w, r, Problem{Status: http.StatusConflict, Code: CodePriceVersionImmutable,
			Detail: "published and submitted prices are never edited in place; create a new price version"})
	case errors.Is(err, domain.ErrPriceVersionInvalidState):
		writeProblem(w, r, Problem{Status: http.StatusConflict, Code: CodePriceVersionInvalidState, Detail: err.Error()})
	case errors.Is(err, domain.ErrContentHashMismatch):
		writeProblem(w, r, Problem{Status: http.StatusConflict, Code: CodePriceBasisChanged, Detail: err.Error()})
	case errors.Is(err, domain.ErrVersionConflict):
		writeProblem(w, r, Problem{Status: http.StatusPreconditionFailed, Code: CodeVersionConflict,
			Detail: "the version changed since it was read; re-read it and retry with the new ETag"})
	case errors.Is(err, domain.ErrSoDViolation):
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeSoDViolation, Detail: err.Error()})
	case errors.Is(err, domain.ErrMeterNotRegistered):
		writeProblem(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: CodeMeterNotRegistered, Detail: err.Error()})
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		writeProblem(w, r, Problem{Status: http.StatusUnprocessableEntity, Code: CodeIdempotencyKeyReused, Detail: err.Error()})
	case errors.Is(err, store.ErrCurrencyMinorUnitsFixed):
		writeProblem(w, r, Problem{Status: http.StatusConflict, Code: CodeCurrencyMinorUnitsFixed, Detail: err.Error()})
	default:
		h.logger.Error("price book request failed", zap.String("path", r.URL.Path), zap.Error(err))
		writeProblem(w, r, Problem{Status: http.StatusInternalServerError, Code: CodeInternal, Detail: "internal error"})
	}
}

// replayOrFail answers an idempotent replay with the existing resource and the
// status the original request received; anything else goes to fail.
func (h *PriceBookHandler) replayOrFail(w http.ResponseWriter, r *http.Request, err error, status int, product bool) {
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Idempotent-Replayed", "true")
	if product {
		p, gerr := h.store.GetProduct(r.Context(), replay.ResourceID, true)
		if gerr != nil {
			h.fail(w, r, gerr)
			return
		}
		writeJSON(w, status, p)
		return
	}
	v, gerr := h.store.GetPriceVersion(r.Context(), replay.ResourceID, true)
	if gerr != nil {
		h.fail(w, r, gerr)
		return
	}
	writeVersion(w, status, v)
}

// ── Currencies ───────────────────────────────────────────────────────────────

type putCurrencyRequest struct {
	MinorUnits  *int  `json:"minor_units"`
	SaleEnabled *bool `json:"sale_enabled"`
}

func (h *PriceBookHandler) ListCurrencies(w http.ResponseWriter, r *http.Request) {
	cs, err := h.store.ListCurrencies(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if cs == nil {
		cs = []domain.CommercialCurrency{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"currencies": cs})
}

// PutCurrency registers a currency's minor units and sale eligibility. It is
// a full-representation PUT and therefore idempotent without a ledger entry.
func (h *PriceBookHandler) PutCurrency(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if !currencyPathPattern.MatchString(code) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "code",
			Detail: "currency code must be three uppercase letters"})
		return
	}
	principal, ok := h.principal(w, r)
	if !ok || !h.authorize(w, r, principal, ActionCurrencyManage) {
		return
	}
	var req putCurrencyRequest
	if _, ok := readBody(w, r, &req, false); !ok {
		return
	}
	if req.MinorUnits == nil || req.SaleEnabled == nil || *req.MinorUnits < 0 || *req.MinorUnits > 4 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "minor_units (0-4) and sale_enabled are both required"})
		return
	}
	c, err := h.store.UpsertCurrency(r.Context(), &domain.CommercialCurrency{
		CurrencyCode: code, MinorUnits: *req.MinorUnits, SaleEnabled: *req.SaleEnabled, UpdatedByPrincipalID: principal,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ── Products ─────────────────────────────────────────────────────────────────

type createProductRequest struct {
	ProductCode string             `json:"product_code"`
	ProductKind domain.ProductKind `json:"product_kind"`
}

func (h *PriceBookHandler) CreateProduct(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok || !h.authorize(w, r, principal, ActionPriceBookPropose) {
		return
	}
	var req createProductRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if err := domain.ValidateProductInput(req.ProductCode, req.ProductKind); err != nil {
		h.fail(w, r, err)
		return
	}
	p := &domain.Product{
		ProductID: domain.NewCommercialID(domain.PrefixProduct), ProductCode: req.ProductCode,
		ProductKind: req.ProductKind, CreatedByPrincipalID: principal,
	}
	cmd, ok := h.newCommand(w, r, principal, "CreateProduct", p.ProductID, raw, false)
	if !ok {
		return
	}
	created, err := h.store.CreateProduct(r.Context(), p, cmd.claim)
	if err != nil {
		h.replayOrFail(w, r, err, http.StatusCreated, true)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// GetPublishedProducts lists what is on sale right now.
func (h *PriceBookHandler) GetPublishedProducts(w http.ResponseWriter, r *http.Request) {
	ps, err := h.store.ListPublishedProducts(r.Context(), h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ps == nil {
		ps = []domain.ProductSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": ps})
}

func (h *PriceBookHandler) GetPriceHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixProduct, chi.URLParam(r, "productID"))
	if !ok {
		return
	}
	vs, err := h.store.ListPriceHistory(r.Context(), id, h.sellerView(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if vs == nil {
		vs = []domain.PriceVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"product_id": id, "versions": vs})
}

// ── Drafting ─────────────────────────────────────────────────────────────────

type createDraftVersionRequest struct {
	ProductID            string     `json:"product_id"`
	DisplayName          string     `json:"display_name"`
	BillingInterval      string     `json:"billing_interval"`
	BillingIntervalCount *int       `json:"billing_interval_count"`
	CurrencyCode         string     `json:"currency_code"`
	MarketCodes          []string   `json:"market_codes"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to"`
	ChangeReason         string     `json:"change_reason"`
	ClonePrevious        bool       `json:"clone_previous"`
}

func (h *PriceBookHandler) CreateDraftVersion(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok || !h.authorize(w, r, principal, ActionPriceBookPropose) {
		return
	}
	var req createDraftVersionRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	productID, ok := parseID(w, r, domain.PrefixProduct, req.ProductID)
	if !ok {
		return
	}
	markets, err := domain.NormalizeMarketCodes(req.MarketCodes)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	count := 1
	if req.BillingIntervalCount != nil {
		count = *req.BillingIntervalCount
	}
	v := &domain.PriceVersion{
		PriceVersionID: domain.NewCommercialID(domain.PrefixPriceVersion), ProductID: productID,
		DisplayName: req.DisplayName, BillingInterval: req.BillingInterval, BillingIntervalCount: count,
		CurrencyCode: req.CurrencyCode, MarketCodes: markets, EffectiveFrom: req.EffectiveFrom.UTC(),
		ChangeReason: req.ChangeReason, CreatedAt: h.now(), CreatedByPrincipalID: principal,
	}
	if req.EffectiveTo != nil {
		t := req.EffectiveTo.UTC()
		v.EffectiveTo = &t
	}
	if err := domain.ValidateDraftHeader(v); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd, ok := h.newCommand(w, r, principal, "CreateDraftVersion", v.PriceVersionID, raw, false)
	if !ok {
		return
	}
	created, err := h.store.CreateDraftVersion(r.Context(), v, req.ClonePrevious, cmd.claim)
	if err != nil {
		h.replayOrFail(w, r, err, http.StatusCreated, false)
		return
	}
	writeVersion(w, http.StatusCreated, created)
}

type priceTierRequest struct {
	UpToQuantity *string `json:"up_to_quantity"`
	UnitAmount   string  `json:"unit_amount"`
	FlatAmount   *string `json:"flat_amount"`
}

type priceComponentRequest struct {
	ComponentType     domain.ComponentType `json:"component_type"`
	Amount            *string              `json:"amount"`
	BillingTiming     *string              `json:"billing_timing"`
	UnitName          *string              `json:"unit_name"`
	IncludedQuantity  *string              `json:"included_quantity"`
	MinimumQuantity   *string              `json:"minimum_quantity"`
	MaximumQuantity   *string              `json:"maximum_quantity"`
	QuantityRounding  *string              `json:"quantity_rounding"`
	TierMode          *string              `json:"tier_mode"`
	Tiers             []priceTierRequest   `json:"tiers"`
	MeterKey          *string              `json:"meter_key"`
	MeterVersion      *int                 `json:"meter_version"`
	AggregationMethod *string              `json:"aggregation_method"`
	TriggerEvent      *string              `json:"trigger_event"`
	EligibilityCode   *string              `json:"eligibility_code"`
	ExpiresAfterDays  *int                 `json:"expires_after_days"`
	DiscountType      *string              `json:"discount_type"`
	DiscountValue     *string              `json:"discount_value"`
	DiscountCapAmount *string              `json:"discount_cap_amount"`
	DurationIntervals *int                 `json:"duration_intervals"`
	RequiresApproval  *bool                `json:"requires_approval"`
}

// PutPriceComponent is AddPriceComponent: it adds the component named by
// {key} to a draft, replacing any component already under that key.
func (h *PriceBookHandler) PutPriceComponent(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixPriceVersion, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	principal, ok := h.principal(w, r)
	if !ok || !h.authorize(w, r, principal, ActionPriceBookPropose) {
		return
	}
	var req priceComponentRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	cmd, ok := h.newCommand(w, r, principal, "AddPriceComponent", id, raw, true)
	if !ok {
		return
	}
	c := &domain.PriceComponent{
		ComponentKey: chi.URLParam(r, "key"), ComponentType: req.ComponentType, Amount: req.Amount,
		BillingTiming: req.BillingTiming, UnitName: req.UnitName, IncludedQuantity: req.IncludedQuantity,
		MinimumQuantity: req.MinimumQuantity, MaximumQuantity: req.MaximumQuantity,
		QuantityRounding: req.QuantityRounding, TierMode: req.TierMode, MeterKey: req.MeterKey,
		MeterVersion: req.MeterVersion, AggregationMethod: req.AggregationMethod, TriggerEvent: req.TriggerEvent,
		EligibilityCode: req.EligibilityCode, ExpiresAfterDays: req.ExpiresAfterDays,
		DiscountType: req.DiscountType, DiscountValue: req.DiscountValue, DiscountCapAmount: req.DiscountCapAmount,
		DurationIntervals: req.DurationIntervals, RequiresApproval: req.RequiresApproval,
		CreatedAt: h.now(), CreatedByPrincipalID: principal,
	}
	for i, t := range req.Tiers {
		c.Tiers = append(c.Tiers, domain.PriceTier{TierIndex: i + 1, UpToQuantity: t.UpToQuantity, UnitAmount: t.UnitAmount, FlatAmount: t.FlatAmount})
	}
	v, err := h.store.PutPriceComponent(r.Context(), id, cmd.ifMatch, c, cmd.claim)
	if err != nil {
		h.replayOrFail(w, r, err, http.StatusOK, false)
		return
	}
	writeVersion(w, http.StatusOK, v)
}

func (h *PriceBookHandler) RemovePriceComponent(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixPriceVersion, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	principal, ok := h.principal(w, r)
	if !ok || !h.authorize(w, r, principal, ActionPriceBookPropose) {
		return
	}
	cmd, ok := h.newCommand(w, r, principal, "RemovePriceComponent", id, nil, true)
	if !ok {
		return
	}
	v, err := h.store.RemovePriceComponent(r.Context(), id, cmd.ifMatch, chi.URLParam(r, "key"), principal, cmd.claim)
	if err != nil {
		h.replayOrFail(w, r, err, http.StatusOK, false)
		return
	}
	writeVersion(w, http.StatusOK, v)
}

type trialRequest struct {
	DurationDays          int    `json:"duration_days"`
	Conversion            string `json:"conversion"`
	PaymentMethodRequired bool   `json:"payment_method_required"`
}

type commercialTermsRequest struct {
	TermsDocumentRef     string        `json:"terms_document_ref"`
	TermsDocumentSHA256  string        `json:"terms_document_sha256"`
	AutoRenew            *bool         `json:"auto_renew"`
	RenewalNoticeDays    *int          `json:"renewal_notice_days"`
	MinimumTermIntervals *int          `json:"minimum_term_intervals"`
	Trial                *trialRequest `json:"trial"`
}

func (h *PriceBookHandler) SetCommercialTerms(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixPriceVersion, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	principal, ok := h.principal(w, r)
	if !ok || !h.authorize(w, r, principal, ActionPriceBookPropose) {
		return
	}
	var req commercialTermsRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if req.AutoRenew == nil || req.RenewalNoticeDays == nil || req.MinimumTermIntervals == nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "auto_renew, renewal_notice_days and minimum_term_intervals are required: terms are never defaulted"})
		return
	}
	t := &domain.CommercialTerms{
		TermsDocumentRef: req.TermsDocumentRef, TermsDocumentSHA256: req.TermsDocumentSHA256,
		AutoRenew: *req.AutoRenew, RenewalNoticeDays: *req.RenewalNoticeDays,
		MinimumTermIntervals: *req.MinimumTermIntervals, SetAt: h.now(), SetByPrincipalID: principal,
	}
	if req.Trial != nil {
		t.Trial = &domain.TrialPolicy{DurationDays: req.Trial.DurationDays, Conversion: req.Trial.Conversion,
			PaymentMethodRequired: req.Trial.PaymentMethodRequired}
	}
	if err := domain.ValidateTerms(t); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd, ok := h.newCommand(w, r, principal, "SetCommercialTerms", id, raw, true)
	if !ok {
		return
	}
	v, err := h.store.SetCommercialTerms(r.Context(), id, cmd.ifMatch, t, cmd.claim)
	if err != nil {
		h.replayOrFail(w, r, err, http.StatusOK, false)
		return
	}
	writeVersion(w, http.StatusOK, v)
}

type capabilitiesRequest struct {
	Capabilities []domain.PlanCapability `json:"capabilities"`
}

func (h *PriceBookHandler) SetPlanCapabilities(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixPriceVersion, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	principal, ok := h.principal(w, r)
	if !ok || !h.authorize(w, r, principal, ActionPriceBookPropose) {
		return
	}
	var req capabilitiesRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if err := domain.ValidateCapabilities(req.Capabilities); err != nil {
		h.fail(w, r, err)
		return
	}
	cmd, ok := h.newCommand(w, r, principal, "SetPlanCapabilities", id, raw, true)
	if !ok {
		return
	}
	v, err := h.store.SetCapabilities(r.Context(), id, cmd.ifMatch, req.Capabilities, principal, cmd.claim)
	if err != nil {
		h.replayOrFail(w, r, err, http.StatusOK, false)
		return
	}
	writeVersion(w, http.StatusOK, v)
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

type lifecycleRequest struct {
	Reason string `json:"reason"`
}

// PriceVersionAction dispatches POST /price-versions/{id}:{action}.
func (h *PriceBookHandler) PriceVersionAction(w http.ResponseWriter, r *http.Request) {
	rawID, action, found := strings.Cut(chi.URLParam(r, "id"), ":")
	if !found {
		writeProblem(w, r, Problem{Status: http.StatusMethodNotAllowed, Code: CodeInvalidCommercialContext,
			Detail: "POST a custom method, e.g. /price-versions/{id}:submit"})
		return
	}
	id, ok := parseID(w, r, domain.PrefixPriceVersion, rawID)
	if !ok {
		return
	}
	permission := map[string]string{
		"submit": ActionPriceBookPropose, "approve": ActionPriceBookApprove, "reject": ActionPriceBookApprove,
		"publish": ActionPriceBookPublish, "retire": ActionPriceBookRetire,
	}[action]
	if permission == "" {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound,
			Detail: "unknown action " + strconv.Quote(action) + "; expected submit, approve, reject, publish or retire"})
		return
	}
	principal, ok := h.principal(w, r)
	if !ok || !h.authorize(w, r, principal, permission) {
		return
	}
	var req lifecycleRequest
	raw, ok := readBody(w, r, &req, true)
	if !ok {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if (action == "reject" || action == "retire") && reason == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason",
			Detail: "a reason is required to " + action + " a price version"})
		return
	}
	cmd, ok := h.newCommand(w, r, principal, "PriceVersion:"+action, id, raw, true)
	if !ok {
		return
	}

	ctx, now := r.Context(), h.now()
	var v *domain.PriceVersion
	var err error
	switch action {
	case "submit":
		v, err = h.store.SubmitForApproval(ctx, id, cmd.ifMatch, principal, now, cmd.claim)
	case "approve":
		v, err = h.store.ApprovePriceVersion(ctx, id, cmd.ifMatch, principal, now, cmd.claim)
	case "reject":
		v, err = h.store.RejectPriceVersion(ctx, id, cmd.ifMatch, principal, reason, now, cmd.claim)
	case "publish":
		v, err = h.store.PublishPriceVersion(ctx, id, cmd.ifMatch, principal, now, cmd.claim)
	case "retire":
		v, err = h.store.RetirePriceVersion(ctx, id, cmd.ifMatch, principal, reason, now, cmd.claim)
	}
	if err != nil {
		h.replayOrFail(w, r, err, http.StatusOK, false)
		return
	}
	writeVersion(w, http.StatusOK, v)
}

// ── Queries ──────────────────────────────────────────────────────────────────

func (h *PriceBookHandler) GetPriceVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixPriceVersion, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	v, err := h.store.GetPriceVersion(r.Context(), id, h.sellerView(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeVersion(w, http.StatusOK, v)
}

func (h *PriceBookHandler) GetPlanCapabilities(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixPriceVersion, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	v, err := h.store.GetPriceVersion(r.Context(), id, h.sellerView(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"price_version_id": v.PriceVersionID, "product_id": v.ProductID, "status": v.Status,
		"capabilities": v.Capabilities,
	})
}

func (h *PriceBookHandler) CompareVersions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	baseID, ok := parseID(w, r, domain.PrefixPriceVersion, q.Get("base"))
	if !ok {
		return
	}
	targetID, ok := parseID(w, r, domain.PrefixPriceVersion, q.Get("target"))
	if !ok {
		return
	}
	seller := h.sellerView(r)
	base, err := h.store.GetPriceVersion(r.Context(), baseID, seller)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	target, err := h.store.GetPriceVersion(r.Context(), targetID, seller)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d, err := domain.CompareVersions(base, target)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ResolveSellableOffers returns the offers a new customer can buy now, as
// resolved by the server. No client-supplied price or version is consulted:
// the caller names what it wants to buy, the server says what it costs.
func (h *PriceBookHandler) ResolveSellableOffers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := domain.SellableOfferFilter{
		ProductCode: q.Get("product_code"), CurrencyCode: q.Get("currency"), MarketCode: q.Get("market"),
	}
	if f.ProductCode != "" && !queryCodePattern.MatchString(f.ProductCode) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "product_code", Detail: "invalid product code"})
		return
	}
	if f.CurrencyCode != "" && !currencyPathPattern.MatchString(f.CurrencyCode) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "currency", Detail: "invalid currency code"})
		return
	}
	if f.MarketCode != "" && !queryMarketPattern.MatchString(f.MarketCode) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "market", Detail: "invalid market code"})
		return
	}
	now := h.now()
	offers, err := h.store.ResolveSellableOffers(r.Context(), f, now)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if offers == nil {
		offers = []domain.PriceVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolved_at": now, "offers": offers})
}
