// COM-02 Subscription HTTP surface (ZS-SVC-Q-001 §4.2, §10).
package handler

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// A customer manages and reads its own subscription at its organization's
// scope. ZoikoSuite operators act across organizations at platform scope: an
// assisted change, and activating a pending subscription, which is seller
// authority — a customer can never grant itself paid access.
const (
	ActionSubscriptionManage   = "COMMERCIAL_SUBSCRIPTION_MANAGE"
	ActionSubscriptionRead     = "COMMERCIAL_SUBSCRIPTION_READ"
	ActionSubscriptionAssist   = "COMMERCIAL_SUBSCRIPTION_ASSIST"
	ActionSubscriptionActivate = "COMMERCIAL_SUBSCRIPTION_ACTIVATE"
	ActionAccountMarketSet     = "COMMERCIAL_ACCOUNT_MARKET_SET"
)

// Stable problem codes for COM-02.
const (
	CodeOfferChanged             = "OFFER_CHANGED"
	CodeTermsNotAccepted         = "TERMS_NOT_ACCEPTED"
	CodePaymentMethodRequired    = "PAYMENT_METHOD_REQUIRED"
	CodeSubscriptionOverlap      = "SUBSCRIPTION_OVERLAP"
	CodeSubscriptionInvalidState = "SUBSCRIPTION_INVALID_STATE"
	CodeSellerActivationRequired = "SELLER_ACTIVATION_REQUIRED"
	CodeMinimumTermNotMet        = "MINIMUM_TERM_NOT_MET"
	CodeRenewalNotDue            = "RENEWAL_NOT_DUE"
	CodeSubscriptionEnded        = "SUBSCRIPTION_ENDED"
	CodeEndpointRetired          = "ENDPOINT_RETIRED"
)

var (
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// A payment method reference is a provider token. Something shaped like a
	// card number is refused before it reaches storage or a log (negative
	// path #30; COM-CTRL-034).
	panLikePattern = regexp.MustCompile(`^[0-9][0-9 -]{11,22}[0-9]$`)
)

// SubscriptionHandler serves COM-02.
type SubscriptionHandler struct {
	store  store.SubscriptionStore
	authz  AuthzChecker
	logger *zap.Logger
	now    func() time.Time
}

func NewSubscriptionHandler(st store.SubscriptionStore, az AuthzChecker, logger *zap.Logger) *SubscriptionHandler {
	return &SubscriptionHandler{store: st, authz: az, logger: logger, now: serverNow}
}

func (h *SubscriptionHandler) WithClock(now func() time.Time) *SubscriptionHandler {
	h.now = now
	return h
}

func RegisterSubscriptionV2Routes(r chi.Router, h *SubscriptionHandler) {
	r.Route("/v1/commercial", func(r chi.Router) {
		r.Put("/accounts/{id}/market", h.SetAccountMarket)
		r.Post("/subscriptions:start", h.StartSubscription)
		r.Post("/subscriptions/{id}", h.SubscriptionAction)
		r.Get("/subscriptions/{id}", h.GetSubscription)
		r.Get("/subscriptions/{id}/effective-version", h.GetEffectiveVersion)
		r.Get("/subscriptions/{id}/history", h.GetChangeHistory)
		r.Get("/subscriptions/{id}/renewal-state", h.GetRenewalState)
	})
}

// ── Scope ────────────────────────────────────────────────────────────────────

type assistedRequest struct {
	OrganizationID   string `json:"organization_id"`
	CustomerBasisRef string `json:"customer_basis_ref"`
}

type subScope struct {
	ctx       context.Context
	org       string
	principal string
	channel   domain.Channel
	basis     *string
}

func (h *SubscriptionHandler) authorize(w http.ResponseWriter, r *http.Request, principal, scope, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, scope, action); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "principal is not granted " + action})
		} else {
			writeProblem(w, r, Problem{Status: http.StatusServiceUnavailable, Code: CodeAuthorizationUnavailable,
				Detail: "authorization service unavailable; the request was refused rather than allowed"})
		}
		return false
	}
	return true
}

// commandScope decides whose subscription a command acts on and under what
// authority. Self-service acts on the verified tenant only. An assisted
// command names the customer's organization, needs the operator's platform
// grant, and must record the customer's basis for the change.
func (h *SubscriptionHandler) commandScope(w http.ResponseWriter, r *http.Request, assisted *assistedRequest, selfAction, operatorAction string) (*subScope, bool) {
	principal := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principal == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Principal-Id is required"})
		return nil, false
	}
	if assisted != nil {
		if _, err := uuid.Parse(assisted.OrganizationID); err != nil {
			writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
				Field: "assisted.organization_id", Detail: "must be the customer's organization id"})
			return nil, false
		}
		basis := strings.TrimSpace(assisted.CustomerBasisRef)
		if basis == "" || len(basis) > 255 {
			writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
				Field: "assisted.customer_basis_ref", Detail: "an assisted change must record the customer's basis for it (at most 255 characters)"})
			return nil, false
		}
		if !h.authorize(w, r, principal, platformScopeID, operatorAction) {
			return nil, false
		}
		return &subScope{ctx: svcmiddleware.WithTenant(r.Context(), assisted.OrganizationID), org: assisted.OrganizationID,
			principal: principal, channel: domain.ChannelAssisted, basis: &basis}, true
	}
	org := svcmiddleware.TenantFromContext(r.Context())
	if org == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated,
			Detail: "X-Tenant-Id is required — the gateway sets it from a verified identity envelope"})
		return nil, false
	}
	if !h.authorize(w, r, principal, org, selfAction) {
		return nil, false
	}
	return &subScope{ctx: r.Context(), org: org, principal: principal, channel: domain.ChannelSelfService}, true
}

// readScope is commandScope for reads: an operator names the organization in
// the query string and needs the platform grant; a customer reads its own.
func (h *SubscriptionHandler) readScope(w http.ResponseWriter, r *http.Request) (*subScope, bool) {
	if org := r.URL.Query().Get("organization_id"); org != "" {
		return h.commandScope(w, r, &assistedRequest{OrganizationID: org, CustomerBasisRef: "read"}, "", ActionSubscriptionAssist)
	}
	return h.commandScope(w, r, nil, ActionSubscriptionRead, "")
}

func (h *SubscriptionHandler) asOf(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	raw := r.URL.Query().Get("at")
	if raw == "" {
		return h.now(), true
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "at", Detail: "must be an RFC 3339 timestamp"})
		return time.Time{}, false
	}
	return t.UTC().Truncate(time.Microsecond), true
}

func writeSubscription(w http.ResponseWriter, status int, v *domain.SubscriptionView) {
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(v.RowVersion)))
	writeJSON(w, status, v)
}

func (h *SubscriptionHandler) replayOrFail(w http.ResponseWriter, r *http.Request, sc *subScope, err error, status int) {
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		writeFailure(w, r, h.logger, err)
		return
	}
	v, gerr := h.store.GetSubscriptionAsOf(sc.ctx, replay.ResourceID, h.now())
	if gerr != nil {
		writeFailure(w, r, h.logger, gerr)
		return
	}
	w.Header().Set("Idempotent-Replayed", "true")
	writeSubscription(w, status, v)
}

// subscriptionFailure maps COM-02 errors; writeFailure consults it for any
// error the shared mapping does not know.
func subscriptionFailure(w http.ResponseWriter, r *http.Request, err error) bool {
	p := Problem{Detail: err.Error()}
	switch {
	case errors.Is(err, domain.ErrCommercialAccountNotFound), errors.Is(err, domain.ErrSubscriptionNotFound),
		errors.Is(err, store.ErrNoEffectiveVersion):
		p.Status, p.Code = http.StatusNotFound, CodeNotFound
	case errors.Is(err, domain.ErrAccountMarketNotSet), errors.Is(err, domain.ErrAccountNotActive),
		errors.Is(err, domain.ErrWrongProductKind):
		p.Status, p.Code = http.StatusUnprocessableEntity, CodeInvalidCommercialContext
	case errors.Is(err, domain.ErrPriceVersionNotSellable):
		p.Status, p.Code = http.StatusUnprocessableEntity, CodePriceVersionInactive
	case errors.Is(err, domain.ErrOfferChanged):
		p.Status, p.Code = http.StatusConflict, CodeOfferChanged
	case errors.Is(err, domain.ErrTermsNotAccepted):
		p.Status, p.Code = http.StatusConflict, CodeTermsNotAccepted
	case errors.Is(err, domain.ErrPaymentMethodRequired):
		p.Status, p.Code = http.StatusUnprocessableEntity, CodePaymentMethodRequired
	case errors.Is(err, domain.ErrSubscriptionOverlap), errors.Is(err, domain.ErrLegacySubscriptionLive):
		p.Status, p.Code = http.StatusConflict, CodeSubscriptionOverlap
	case errors.Is(err, domain.ErrSubscriptionInvalidState), errors.Is(err, domain.ErrConversionAlreadyScheduled):
		p.Status, p.Code = http.StatusConflict, CodeSubscriptionInvalidState
	case errors.Is(err, domain.ErrSellerActivationRequired):
		p.Status, p.Code = http.StatusForbidden, CodeSellerActivationRequired
	case errors.Is(err, domain.ErrMinimumTermNotMet):
		p.Status, p.Code = http.StatusConflict, CodeMinimumTermNotMet
	case errors.Is(err, domain.ErrRenewalNotDue):
		p.Status, p.Code = http.StatusConflict, CodeRenewalNotDue
	case errors.Is(err, domain.ErrSubscriptionEnded):
		p.Status, p.Code = http.StatusConflict, CodeSubscriptionEnded
	default:
		return false
	}
	writeProblem(w, r, p)
	return true
}

// ── Account market ───────────────────────────────────────────────────────────

type setMarketRequest struct {
	OrganizationID string `json:"organization_id"`
	MarketCode     string `json:"market_code"`
}

// SetAccountMarket records the market a customer buys in. Seller authority
// only; it changes which offers are sold to the account from now on and
// never touches subscriptions already bound.
func (h *SubscriptionHandler) SetAccountMarket(w http.ResponseWriter, r *http.Request) {
	accountID := chi.URLParam(r, "id")
	if _, err := uuid.Parse(accountID); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Detail: "invalid commercial account id"})
		return
	}
	var req setMarketRequest
	if _, ok := readBody(w, r, &req, false); !ok {
		return
	}
	if !queryMarketPattern.MatchString(req.MarketCode) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "market_code", Detail: "must be 2-8 uppercase letters"})
		return
	}
	sc, ok := h.commandScope(w, r, &assistedRequest{OrganizationID: req.OrganizationID, CustomerBasisRef: "account market"}, "", ActionAccountMarketSet)
	if !ok {
		return
	}
	if err := h.store.SetAccountMarket(sc.ctx, accountID, req.MarketCode, sc.principal, h.now()); err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"commercial_account_id": accountID, "market_code": req.MarketCode})
}

// ── StartSubscription ────────────────────────────────────────────────────────

type addOnRequest struct {
	ProductCode         string            `json:"product_code"`
	Quantities          map[string]string `json:"quantities"`
	AcceptedTermsSHA256 string            `json:"accepted_terms_sha256"`
}

type startSubscriptionRequest struct {
	CommercialAccountID    string            `json:"commercial_account_id"`
	ProductCode            string            `json:"product_code"`
	Quantities             map[string]string `json:"quantities"`
	AcceptedTermsSHA256    string            `json:"accepted_terms_sha256"`
	ExpectedPriceVersionID *string           `json:"expected_price_version_id"`
	AddOns                 []addOnRequest    `json:"add_ons"`
	StartsAt               *time.Time        `json:"starts_at"`
	PaymentMethodRef       *string           `json:"payment_method_ref"`
	Assisted               *assistedRequest  `json:"assisted"`
}

func (h *SubscriptionHandler) StartSubscription(w http.ResponseWriter, r *http.Request) {
	var req startSubscriptionRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	sc, ok := h.commandScope(w, r, req.Assisted, ActionSubscriptionManage, ActionSubscriptionAssist)
	if !ok {
		return
	}
	bad := func(field, detail string) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: field, Detail: detail})
	}
	if _, err := uuid.Parse(req.CommercialAccountID); err != nil {
		bad("commercial_account_id", "must be the commercial account id")
		return
	}
	if !queryCodePattern.MatchString(req.ProductCode) {
		bad("product_code", "invalid product code")
		return
	}
	if !sha256Pattern.MatchString(req.AcceptedTermsSHA256) {
		bad("accepted_terms_sha256", "must be the SHA-256 of the terms the customer accepted")
		return
	}
	if req.ExpectedPriceVersionID != nil {
		if _, ok := parseID(w, r, domain.PrefixPriceVersion, *req.ExpectedPriceVersionID); !ok {
			return
		}
	}
	if req.PaymentMethodRef != nil {
		ref := strings.TrimSpace(*req.PaymentMethodRef)
		if ref == "" || len(ref) > 255 || panLikePattern.MatchString(ref) {
			bad("payment_method_ref", "must be a payment provider token, never a card number")
			return
		}
		req.PaymentMethodRef = &ref
	}
	now := h.now()
	startsAt := now
	if req.StartsAt != nil {
		startsAt = req.StartsAt.UTC().Truncate(time.Microsecond)
		if startsAt.Before(now) {
			bad("starts_at", "cannot be in the past")
			return
		}
	}
	p := domain.StartSubscriptionParams{
		SubscriptionID: domain.NewCommercialID(domain.PrefixSubscription), ChangeID: domain.NewCommercialID(domain.PrefixCommercialChange),
		CommercialAccountID: req.CommercialAccountID, PlanProductCode: req.ProductCode, Quantities: req.Quantities,
		AcceptedTermsSHA256: req.AcceptedTermsSHA256, ExpectedPriceVersionID: req.ExpectedPriceVersionID,
		StartsAt: startsAt, Channel: sc.channel, CustomerBasisRef: sc.basis, PaymentMethodRef: req.PaymentMethodRef,
		Actor: sc.principal, Now: now,
	}
	if p.Quantities == nil {
		p.Quantities = map[string]string{}
	}
	for i, a := range req.AddOns {
		if !queryCodePattern.MatchString(a.ProductCode) || !sha256Pattern.MatchString(a.AcceptedTermsSHA256) {
			bad("add_ons["+strconv.Itoa(i)+"]", "each add-on needs a product_code and the accepted_terms_sha256 of its terms")
			return
		}
		q := a.Quantities
		if q == nil {
			q = map[string]string{}
		}
		p.AddOns = append(p.AddOns, domain.AddOnSelection{ProductCode: a.ProductCode, Quantities: q, AcceptedTermsSHA256: a.AcceptedTermsSHA256})
	}
	cmd, ok := commandFor(w, r, sc.org, sc.principal, "StartSubscription", p.SubscriptionID, raw, false)
	if !ok {
		return
	}
	v, err := h.store.StartSubscription(sc.ctx, p, cmd.claim)
	if err != nil {
		h.replayOrFail(w, r, sc, err, http.StatusCreated)
		return
	}
	writeSubscription(w, http.StatusCreated, v)
}

// ── Lifecycle commands ───────────────────────────────────────────────────────

type subscriptionActionRequest struct {
	Reason   string           `json:"reason"`
	Assisted *assistedRequest `json:"assisted"`
}

// SubscriptionAction dispatches POST /subscriptions/{id}:{action}.
func (h *SubscriptionHandler) SubscriptionAction(w http.ResponseWriter, r *http.Request) {
	rawID, action, found := strings.Cut(chi.URLParam(r, "id"), ":")
	if !found {
		writeProblem(w, r, Problem{Status: http.StatusMethodNotAllowed, Code: CodeInvalidCommercialContext,
			Detail: "POST a custom method, e.g. /subscriptions/{id}:schedule-cancellation"})
		return
	}
	id, ok := parseID(w, r, domain.PrefixSubscription, rawID)
	if !ok {
		return
	}
	operatorAction := map[string]string{
		"activate": ActionSubscriptionActivate, "schedule-cancellation": ActionSubscriptionAssist,
		"cancel": ActionSubscriptionAssist, "reactivate": ActionSubscriptionAssist, "renew": ActionSubscriptionAssist,
	}[action]
	if operatorAction == "" {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound,
			Detail: "unknown action " + strconv.Quote(action) + "; expected activate, schedule-cancellation, cancel, reactivate or renew"})
		return
	}
	var req subscriptionActionRequest
	raw, ok := readBody(w, r, &req, true)
	if !ok {
		return
	}
	if len(req.Reason) > 1000 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "at most 1000 characters"})
		return
	}
	sc, ok := h.commandScope(w, r, req.Assisted, ActionSubscriptionManage, operatorAction)
	if !ok {
		return
	}
	cmd, ok := commandFor(w, r, sc.org, sc.principal, "Subscription:"+action, id, raw, true)
	if !ok {
		return
	}
	c := domain.SubscriptionCommand{
		SubscriptionID: id, ExpectedVersion: cmd.ifMatch, ChangeID: domain.NewCommercialID(domain.PrefixCommercialChange),
		Actor: sc.principal, Channel: sc.channel, CustomerBasisRef: sc.basis, Reason: strings.TrimSpace(req.Reason), Now: h.now(),
	}
	var v *domain.SubscriptionView
	var err error
	switch action {
	case "activate":
		// Without the operator's activation grant this can only be the
		// customer confirming a trial conversion; the store refuses it for a
		// PENDING subscription.
		v, err = h.store.ActivateSubscription(sc.ctx, c, sc.channel == domain.ChannelSelfService, cmd.claim)
	case "schedule-cancellation":
		v, err = h.store.ScheduleCancellation(sc.ctx, c, cmd.claim)
	case "cancel":
		v, err = h.store.CancelNow(sc.ctx, c, cmd.claim)
	case "reactivate":
		v, err = h.store.Reactivate(sc.ctx, c, cmd.claim)
	case "renew":
		v, err = h.store.Renew(sc.ctx, c, cmd.claim)
	}
	if err != nil {
		h.replayOrFail(w, r, sc, err, http.StatusOK)
		return
	}
	writeSubscription(w, http.StatusOK, v)
}

// ── Queries ──────────────────────────────────────────────────────────────────

func (h *SubscriptionHandler) subscriptionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	return parseID(w, r, domain.PrefixSubscription, chi.URLParam(r, "id"))
}

func (h *SubscriptionHandler) GetSubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := h.subscriptionID(w, r)
	if !ok {
		return
	}
	at, ok := h.asOf(w, r)
	if !ok {
		return
	}
	sc, ok := h.readScope(w, r)
	if !ok {
		return
	}
	v, err := h.store.GetSubscriptionAsOf(sc.ctx, id, at)
	if err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	writeSubscription(w, http.StatusOK, v)
}

// GetEffectiveVersion reconstructs the agreement in force at ?at= (default:
// now). Historical reads are the point: an as-of query never grants access.
func (h *SubscriptionHandler) GetEffectiveVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := h.subscriptionID(w, r)
	if !ok {
		return
	}
	at, ok := h.asOf(w, r)
	if !ok {
		return
	}
	sc, ok := h.readScope(w, r)
	if !ok {
		return
	}
	v, err := h.store.GetEffectiveVersion(sc.ctx, id, at)
	if err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"as_of": at, "version": v})
}

func (h *SubscriptionHandler) GetChangeHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := h.subscriptionID(w, r)
	if !ok {
		return
	}
	sc, ok := h.readScope(w, r)
	if !ok {
		return
	}
	vs, err := h.store.GetChangeHistory(sc.ctx, id)
	if err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	if vs == nil {
		vs = []domain.SubscriptionVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription_id": id, "versions": vs})
}

func (h *SubscriptionHandler) GetRenewalState(w http.ResponseWriter, r *http.Request) {
	id, ok := h.subscriptionID(w, r)
	if !ok {
		return
	}
	sc, ok := h.readScope(w, r)
	if !ok {
		return
	}
	rs, err := h.store.GetRenewalState(sc.ctx, id, h.now())
	if err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}
