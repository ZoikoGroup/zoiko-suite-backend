// COM-02 part 2c HTTP surface: discount applications and price migration
// offers.
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/store"
)

// Discounts are a sales authority (propose) and a separate commercial or
// finance authority (approve); the store also refuses a self-approval
// whatever grants a principal holds.
const (
	ActionDiscountPropose = "COMMERCIAL_DISCOUNT_PROPOSE"
	ActionDiscountApprove = "COMMERCIAL_DISCOUNT_APPROVE"
)

const (
	CodeDiscountNotApplicable       = "DISCOUNT_NOT_APPLICABLE"
	CodeDiscountExists              = "DISCOUNT_EXISTS"
	CodeDiscountInvalidState        = "DISCOUNT_INVALID_STATE"
	CodeMigrationEligibilityMissing = "MIGRATION_ELIGIBILITY_MISSING"
	CodeNotEligibleForMigration     = "NOT_ELIGIBLE_FOR_MIGRATION"
	CodeMigrationOfferInvalidState  = "MIGRATION_OFFER_INVALID_STATE"
)

// governanceFailure maps 2c errors; subscriptionFailure consults it last.
func governanceFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, domain.ErrDiscountNotApplicable):
		return http.StatusUnprocessableEntity, CodeDiscountNotApplicable, true
	case errors.Is(err, domain.ErrDiscountExists):
		return http.StatusConflict, CodeDiscountExists, true
	case errors.Is(err, domain.ErrDiscountNotFound), errors.Is(err, domain.ErrMigrationOfferNotFound):
		return http.StatusNotFound, CodeNotFound, true
	case errors.Is(err, domain.ErrDiscountInvalidState):
		return http.StatusConflict, CodeDiscountInvalidState, true
	case errors.Is(err, domain.ErrDiscountNotRequester):
		return http.StatusForbidden, CodeAuthorizationDenied, true
	case errors.Is(err, domain.ErrMigrationEligibilityMissing):
		return http.StatusUnprocessableEntity, CodeMigrationEligibilityMissing, true
	case errors.Is(err, domain.ErrNotEligibleForMigration):
		return http.StatusConflict, CodeNotEligibleForMigration, true
	case errors.Is(err, domain.ErrMigrationOfferInvalidState):
		return http.StatusConflict, CodeMigrationOfferInvalidState, true
	case errors.Is(err, domain.ErrMigrationTargetInvalid):
		return http.StatusUnprocessableEntity, CodeInvalidCommercialContext, true
	}
	return 0, "", false
}

// WithGovernance attaches the discount, migration-offer and boundary store.
func (h *SubscriptionHandler) WithGovernance(gov store.GovernanceStore) *SubscriptionHandler {
	h.gov = gov
	return h
}

// ── Discounts ────────────────────────────────────────────────────────────────

type proposeDiscountRequest struct {
	ProductCode  string           `json:"product_code"`
	ComponentKey string           `json:"component_key"`
	Reason       string           `json:"reason"`
	Assisted     *assistedRequest `json:"assisted"`
}

// ProposeDiscount is a sales action on a customer's subscription: always
// assisted, always with the customer's basis recorded.
func (h *SubscriptionHandler) ProposeDiscount(w http.ResponseWriter, r *http.Request) {
	id, ok := h.subscriptionID(w, r)
	if !ok {
		return
	}
	var req proposeDiscountRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if req.Assisted == nil {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "assisted",
			Detail: "a discount is proposed by ZoikoSuite sales on the customer's behalf; name the organization and the customer basis"})
		return
	}
	if !queryCodePattern.MatchString(req.ProductCode) || !queryCodePattern.MatchString(req.ComponentKey) || strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "product_code, component_key and reason are required"})
		return
	}
	sc, ok := h.commandScope(w, r, req.Assisted, "", ActionDiscountPropose)
	if !ok {
		return
	}
	p := domain.ProposeDiscountParams{DiscountApplicationID: domain.NewCommercialID(domain.PrefixDiscountApplication),
		SubscriptionID: id, ProductCode: req.ProductCode, ComponentKey: req.ComponentKey, Reason: strings.TrimSpace(req.Reason),
		CustomerBasisRef: *sc.basis, Actor: sc.principal, Now: h.now()}
	cmd, ok := commandFor(w, r, sc.org, sc.principal, "ProposeDiscount", p.DiscountApplicationID, raw, false)
	if !ok {
		return
	}
	d, err := h.gov.ProposeDiscount(sc.ctx, p, cmd.claim)
	if err != nil {
		h.discountReplayOrFail(w, r, sc, id, err, http.StatusCreated)
		return
	}
	writeDiscount(w, http.StatusCreated, d)
}

func writeDiscount(w http.ResponseWriter, status int, d *domain.DiscountApplication) {
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(d.RowVersion)))
	writeJSON(w, status, d)
}

func (h *SubscriptionHandler) discountReplayOrFail(w http.ResponseWriter, r *http.Request, sc *subScope, subID string, err error, status int) {
	var replay *domain.IdempotentReplayError
	if errors.As(err, &replay) {
		if ds, lerr := h.gov.ListDiscounts(sc.ctx, subID); lerr == nil {
			for i := range ds {
				if ds[i].DiscountApplicationID == replay.ResourceID {
					w.Header().Set("Idempotent-Replayed", "true")
					writeDiscount(w, status, &ds[i])
					return
				}
			}
		}
	}
	writeFailure(w, r, h.logger, err)
}

// DiscountAction dispatches POST /subscriptions/{id}/discounts/{discountID}:{approve|reject|withdraw}.
func (h *SubscriptionHandler) DiscountAction(w http.ResponseWriter, r *http.Request) {
	subID, ok := h.subscriptionID(w, r)
	if !ok {
		return
	}
	rawID, action, found := strings.Cut(chi.URLParam(r, "discountID"), ":")
	permission := map[string]string{
		store.DecisionApprove: ActionDiscountApprove, store.DecisionReject: ActionDiscountApprove, store.DecisionWithdraw: ActionDiscountPropose,
	}[action]
	if !found || permission == "" {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "expected :approve, :reject or :withdraw"})
		return
	}
	discountID, ok := parseID(w, r, domain.PrefixDiscountApplication, rawID)
	if !ok {
		return
	}
	var req subscriptionActionRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if req.Assisted == nil || req.carriesChange() {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "a discount decision takes assisted (organization and basis) and, to reject or withdraw, a reason"})
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if action != store.DecisionApprove && reason == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "a reason is required"})
		return
	}
	sc, ok := h.commandScope(w, r, req.Assisted, "", permission)
	if !ok {
		return
	}
	cmd, ok := commandFor(w, r, sc.org, sc.principal, "Discount:"+action, discountID, raw, true)
	if !ok {
		return
	}
	d, err := h.gov.DecideDiscount(sc.ctx, subID, discountID, cmd.ifMatch, action, sc.principal, reason, h.now(), cmd.claim)
	if err != nil {
		h.discountReplayOrFail(w, r, sc, subID, err, http.StatusOK)
		return
	}
	writeDiscount(w, http.StatusOK, d)
}

func (h *SubscriptionHandler) ListDiscounts(w http.ResponseWriter, r *http.Request) {
	id, ok := h.subscriptionID(w, r)
	if !ok {
		return
	}
	sc, ok := h.readScope(w, r)
	if !ok {
		return
	}
	ds, err := h.gov.ListDiscounts(sc.ctx, id)
	if err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	if ds == nil {
		ds = []domain.DiscountApplication{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription_id": id, "discounts": ds})
}

// ── Migration offers ─────────────────────────────────────────────────────────

type migrationOfferRequest struct {
	FromPriceVersionID    string     `json:"from_price_version_id"`
	ToPriceVersionID      string     `json:"to_price_version_id"`
	EligibilityMode       *string    `json:"eligibility_mode"`
	TargetSubscriptionIDs []string   `json:"target_subscription_ids"`
	AcceptBy              *time.Time `json:"accept_by"`
	Reason                string     `json:"reason"`
}

func writeMigrationOffer(w http.ResponseWriter, status int, o *domain.MigrationOffer) {
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(o.RowVersion)))
	writeJSON(w, status, o)
}

// CreateMigrationOffer drafts an offer. It may be drafted without an
// eligibility rule; it cannot be published without one (#38).
func (h *SubscriptionHandler) CreateMigrationOffer(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.sellerPrincipal(w, r, ActionPriceBookPropose)
	if !ok {
		return
	}
	var req migrationOfferRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	from, ok := parseID(w, r, domain.PrefixPriceVersion, req.FromPriceVersionID)
	if !ok {
		return
	}
	to, ok := parseID(w, r, domain.PrefixPriceVersion, req.ToPriceVersionID)
	if !ok {
		return
	}
	for _, sub := range req.TargetSubscriptionIDs {
		if _, ok := parseID(w, r, domain.PrefixSubscription, sub); !ok {
			return
		}
	}
	o := &domain.MigrationOffer{MigrationOfferID: domain.NewCommercialID(domain.PrefixMigrationOffer), FromPriceVersionID: from,
		ToPriceVersionID: to, EligibilityMode: req.EligibilityMode, TargetSubscriptionIDs: req.TargetSubscriptionIDs,
		Reason: strings.TrimSpace(req.Reason), CreatedAt: h.now(), CreatedByPrincipalID: principal}
	if req.AcceptBy != nil {
		t := req.AcceptBy.UTC().Truncate(time.Microsecond)
		o.AcceptBy = &t
	}
	if err := domain.ValidateMigrationOffer(o); err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "CreateMigrationOffer", o.MigrationOfferID, raw, false)
	if !ok {
		return
	}
	created, err := h.gov.CreateMigrationOffer(r.Context(), o, cmd.claim)
	if err != nil {
		h.migrationReplayOrFail(w, r, err, http.StatusCreated)
		return
	}
	writeMigrationOffer(w, http.StatusCreated, created)
}

func (h *SubscriptionHandler) migrationReplayOrFail(w http.ResponseWriter, r *http.Request, err error, status int) {
	var replay *domain.IdempotentReplayError
	if errors.As(err, &replay) {
		if o, gerr := h.gov.GetMigrationOffer(r.Context(), replay.ResourceID); gerr == nil {
			w.Header().Set("Idempotent-Replayed", "true")
			writeMigrationOffer(w, status, o)
			return
		}
	}
	writeFailure(w, r, h.logger, err)
}

func (h *SubscriptionHandler) GetMigrationOffer(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r, domain.PrefixMigrationOffer, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if _, ok := h.sellerPrincipal(w, r, ActionPriceBookRead); !ok {
		return
	}
	o, err := h.gov.GetMigrationOffer(r.Context(), id)
	if err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	writeMigrationOffer(w, http.StatusOK, o)
}

// MigrationOfferAction dispatches POST /migration-offers/{id}:{publish|withdraw}.
func (h *SubscriptionHandler) MigrationOfferAction(w http.ResponseWriter, r *http.Request) {
	rawID, action, found := strings.Cut(chi.URLParam(r, "id"), ":")
	if !found || (action != "publish" && action != "withdraw") {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "expected :publish or :withdraw"})
		return
	}
	id, ok := parseID(w, r, domain.PrefixMigrationOffer, rawID)
	if !ok {
		return
	}
	principal, ok := h.sellerPrincipal(w, r, ActionPriceBookPublish)
	if !ok {
		return
	}
	var req lifecycleRequest
	raw, ok := readBody(w, r, &req, true)
	if !ok {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if action == "withdraw" && reason == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "a reason is required to withdraw an offer"})
		return
	}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "MigrationOffer:"+action, id, raw, true)
	if !ok {
		return
	}
	var o *domain.MigrationOffer
	var err error
	if action == "publish" {
		o, err = h.gov.PublishMigrationOffer(r.Context(), id, cmd.ifMatch, principal, h.now(), cmd.claim)
	} else {
		o, err = h.gov.WithdrawMigrationOffer(r.Context(), id, cmd.ifMatch, principal, reason, h.now(), cmd.claim)
	}
	if err != nil {
		h.migrationReplayOrFail(w, r, err, http.StatusOK)
		return
	}
	writeMigrationOffer(w, http.StatusOK, o)
}

// ListMigrationOffersFor shows a customer the offers open to its
// subscription.
func (h *SubscriptionHandler) ListMigrationOffersFor(w http.ResponseWriter, r *http.Request) {
	id, ok := h.subscriptionID(w, r)
	if !ok {
		return
	}
	sc, ok := h.readScope(w, r)
	if !ok {
		return
	}
	offers, err := h.gov.ListMigrationOffersFor(sc.ctx, id, h.now())
	if err != nil {
		writeFailure(w, r, h.logger, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription_id": id, "offers": offers})
}
