package ar01

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// cmdBase is the envelope shared by every command on an existing profile.
// legal_entity_id is optional: the entity is derived from the stored profile and
// a body entity that disagrees is refused (spec invariant 16).
type cmdBase struct {
	ProfileID       string `json:"profile_id"`
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	ExpectedVersion *int   `json:"expected_version"`
	ReasonCode      string `json:"reason_code,omitempty"`
	EvidenceID      string `json:"evidence_id,omitempty"`
}

// prepare identifies the caller, loads the profile to learn its entity, then runs
// Guard (tenant+entity reconciliation before authz) for the action.
func (m *Module) prepare(w http.ResponseWriter, r *http.Request, b *cmdBase, action string) (kit.Caller, *Header, bool) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	if err := mustUUID("profile_id", b.ProfileID); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return kit.Caller{}, nil, false
	}
	var h *Header
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = getHeader(r.Context(), tx, c.TenantID, b.ProfileID, false)
		return err
	}); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.LegalEntityID != "" && b.LegalEntityID != h.LegalEntityID {
		m.fail(w, kit.Forbidden(kit.CodeTenantScope, "legal_entity_id does not match the profile's legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok = m.deps.Guard(w, r, h.LegalEntityID, action)
	if !ok {
		return kit.Caller{}, nil, false
	}
	return c, h, true
}

// lock re-reads the header FOR UPDATE inside the command transaction and applies
// the optimistic-concurrency check.
func lock(ctx context.Context, tx pgx.Tx, c kit.Caller, b *cmdBase) (*Header, error) {
	h, err := getHeader(ctx, tx, c.TenantID, b.ProfileID, true)
	if err != nil {
		return nil, err
	}
	if h.Version != *b.ExpectedVersion {
		return nil, kit.ErrStaleVersion
	}
	return h, nil
}

func (m *Module) respond(ctx context.Context, tx pgx.Tx, c kit.Caller, h *Header) (*Profile, error) {
	return loadProfile(ctx, tx, c.TenantID, h, m.today(), nil)
}

func evPayload(c kit.Caller, h *Header, extra map[string]any) map[string]any {
	p := map[string]any{"profile_id": h.ProfileID, "customer_party_id": h.CustomerPartyID, "legal_entity_id": h.LegalEntityID,
		"version": h.Version, "state": h.State, "credit_hold": h.CreditHold, "correlation_id": c.CorrelationID}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func attrDiff(old, nw Attributes) (prior, next map[string]any) {
	prior, next = map[string]any{}, map[string]any{}
	if old.CreditLimit != nw.CreditLimit {
		prior["credit_limit"], next["credit_limit"] = old.CreditLimit.String(), nw.CreditLimit.String()
	}
	if old.CreditCurrency != nw.CreditCurrency {
		prior["credit_currency"], next["credit_currency"] = old.CreditCurrency, nw.CreditCurrency
	}
	if old.InvoicePreferences != nw.InvoicePreferences {
		prior["invoice_preferences"], next["invoice_preferences"] = old.InvoicePreferences, nw.InvoicePreferences
	}
	if old.StatementPreferences != nw.StatementPreferences {
		prior["statement_preferences"], next["statement_preferences"] = old.StatementPreferences, nw.StatementPreferences
	}
	if old.DunningProfile != nw.DunningProfile {
		prior["dunning_profile"], next["dunning_profile"] = old.DunningProfile, nw.DunningProfile
	}
	if old.ARAccountPolicy != nw.ARAccountPolicy {
		prior["ar_account_policy"], next["ar_account_policy"] = old.ARAccountPolicy, nw.ARAccountPolicy
	}
	if !riskEqual(old.RiskAttributes, nw.RiskAttributes) {
		prior["risk_attributes"], next["risk_attributes"] = old.RiskAttributes, nw.RiskAttributes
	}
	return prior, next
}

func riskEqual(a, b RiskAttributes) bool {
	if a.Rating != b.Rating || a.Source != b.Source || a.SourceRef != b.SourceRef || a.AssessedOn != b.AssessedOn {
		return false
	}
	if (a.Score == nil) != (b.Score == nil) {
		return false
	}
	return a.Score == nil || *a.Score == *b.Score
}

// effectiveForChange is when an immediate attribute change takes effect: today,
// or the profile's own start date when it has not started yet.
func effectiveForChange(today string, head *AttrVersion) string {
	if head.EffectiveFrom > today {
		return head.EffectiveFrom
	}
	return today
}

// ── CreateFinancialProfile ───────────────────────────────────────────────────

type createReq struct {
	LegalEntityID        string         `json:"legal_entity_id"`
	CustomerPartyID      string         `json:"customer_party_id"`
	PaymentTermsCode     string         `json:"payment_terms_code"`
	PaymentTermsDays     int            `json:"payment_terms_days"`
	CreditLimit          kit.Amount     `json:"credit_limit"`
	CreditCurrency       string         `json:"credit_currency"`
	CreditLimitPolicyMax kit.Amount     `json:"credit_limit_policy_max"`
	InvoicePreferences   InvoicePrefs   `json:"invoice_preferences"`
	StatementPreferences StatementPrefs `json:"statement_preferences"`
	DunningProfile       string         `json:"dunning_profile"`
	ARAccountPolicy      string         `json:"ar_account_policy"`
	RiskAttributes       RiskAttributes `json:"risk_attributes"`
	EffectiveFrom        string         `json:"effective_from"`
	ReasonCode           string         `json:"reason_code"`
	EvidenceID           string         `json:"evidence_id"`
}

func (m *Module) createFinancialProfile(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !kit.Decode(w, r, &req) {
		return
	}
	attrs := Attributes{CreditLimit: req.CreditLimit, CreditCurrency: req.CreditCurrency, InvoicePreferences: req.InvoicePreferences,
		StatementPreferences: req.StatementPreferences, DunningProfile: req.DunningProfile, ARAccountPolicy: req.ARAccountPolicy,
		RiskAttributes: req.RiskAttributes}
	for _, v := range []error{mustUUID("customer_party_id", req.CustomerPartyID), ValidateTerms(req.PaymentTermsCode, req.PaymentTermsDays), attrs.Validate()} {
		if v != nil {
			m.fail(w, v)
			return
		}
	}
	if req.CreditLimitPolicyMax.IsNegative() || !req.CreditLimitPolicyMax.FitsCurrency(req.CreditCurrency) {
		m.fail(w, kit.Invalid("credit_limit_policy_max must be non-negative and fit the credit currency"))
		return
	}
	today := m.today()
	eff := req.EffectiveFrom
	if eff == "" {
		eff = today
	} else if _, err := ParseDay(eff); err != nil {
		m.fail(w, err)
		return
	} else if eff < today {
		m.fail(w, kit.Invalid("effective_from must not be in the past"))
		return
	}
	if !risksHaveReason(req.RiskAttributes, req.ReasonCode) {
		m.fail(w, kit.Err(http.StatusBadRequest, kit.CodeReasonRequired, "reason_code is required when risk attributes are asserted"))
		return
	}
	if req.CreditLimit > req.CreditLimitPolicyMax && !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionProfileManage)
	if !ok {
		return
	}
	if req.CreditLimit.IsPositive() && !m.deps.Authorize(w, r, c, ActionLimitManage) {
		return
	}
	if req.CreditLimitPolicyMax.IsPositive() && !m.deps.Authorize(w, r, c, ActionLimitApprove) {
		return
	}
	if err := m.requireParty(r.Context(), c, req.CustomerPartyID); err != nil {
		m.fail(w, err)
		return
	}

	var out *Profile
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		ctx := r.Context()
		id := uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO ar01_financial_profiles (profile_id, tenant_id, legal_entity_id, customer_party_id, state,
				credit_limit_policy_max, created_by) VALUES ($1,$2,$3,$4,'DRAFT',$5::numeric,$6)`,
			id, c.TenantID, c.LegalEntityID, req.CustomerPartyID, req.CreditLimitPolicyMax.String(), c.PrincipalID); err != nil {
			if kit.IsUniqueViolation(err, "uq_ar01_profile_live") {
				return kit.Conflict(CodeProfileExists, "a live financial profile already exists for this customer in this legal entity")
			}
			return kit.MapPgErr(err)
		}
		// An initial limit above the profile's policy ceiling starts at zero and
		// waits for a different principal's approval, exactly like a later increase.
		initial := attrs
		pending := req.CreditLimit.IsPositive() && req.CreditLimit > req.CreditLimitPolicyMax
		if pending {
			initial.CreditLimit = 0
		}
		if err := insertAttr(ctx, tx, c, id, 1, eff, "CREATE", req.ReasonCode, req.EvidenceID, "", "", initial); err != nil {
			return err
		}
		if err := insertTerm(ctx, tx, c, id, 1, req.PaymentTermsCode, req.PaymentTermsDays, eff, req.ReasonCode, req.EvidenceID); err != nil {
			return err
		}
		events := []kit.Event{}
		var reqID string
		if pending {
			reqID = uuid.NewString()
			if err := insertLimitRequest(ctx, tx, c, id, reqID, req.CreditLimit, req.CreditCurrency, 0, req.CreditLimitPolicyMax, req.ReasonCode, req.EvidenceID); err != nil {
				return err
			}
		}
		cur := &Header{ProfileID: id, LegalEntityID: c.LegalEntityID, CustomerPartyID: req.CustomerPartyID, State: StateDraft, Version: 1,
			PolicyMax: req.CreditLimitPolicyMax}
		newVals := map[string]any{"payment_terms_code": req.PaymentTermsCode, "payment_terms_days": req.PaymentTermsDays,
			"effective_from": eff, "credit_limit_policy_max": req.CreditLimitPolicyMax.String(), "credit_limit": initial.CreditLimit.String(),
			"credit_currency": req.CreditCurrency, "state": StateDraft}
		if pending {
			newVals["credit_limit_requested"] = req.CreditLimit.String()
			newVals["credit_limit_request_id"] = reqID
		}
		h, err := commitCreate(ctx, tx, c, cur, hist{At: m.deps.Now(), Command: "createFinancialProfile", Reason: req.ReasonCode, Evidence: req.EvidenceID,
			EffectiveFrom: eff, New: newVals}, func(h *Header) []kit.Event {
			events = append(events, kit.Event{AggregateType: AggregateType, AggregateID: h.ProfileID, EventType: EventProfileCreated,
				Payload: evPayload(c, h, map[string]any{"payment_terms_code": req.PaymentTermsCode, "payment_terms_days": req.PaymentTermsDays,
					"credit_limit": initial.CreditLimit.String(), "credit_currency": req.CreditCurrency, "effective_from": eff})})
			if pending {
				events = append(events, limitRequestedEvent(c, h, reqID, req.CreditLimit, req.CreditCurrency, 0))
			}
			return events
		})
		if err != nil {
			return err
		}
		out, err = m.respond(ctx, tx, c, h)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, out)
}

func risksHaveReason(r RiskAttributes, reason string) bool {
	return r.IsZero() || strings.TrimSpace(reason) != ""
}

// commitCreate records the creation history/audit/events for a row already at
// version 1 (no version bump on creation).
func commitCreate(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, h hist, events func(*Header) []kit.Event) (*Header, error) {
	// Creation stays at version 1: write history/audit/events directly.
	nh := *cur
	if _, err := tx.Exec(ctx, `INSERT INTO ar01_profile_history (history_id, tenant_id, legal_entity_id, profile_id, profile_version, command,
			state_after, credit_hold_after, actor_id, reason_code, evidence_id, effective_from, prior_values, new_values, policy_ref, correlation_id, recorded_at)
		VALUES ($1,$2,$3,$4,1,$5,'DRAFT',false,$6,NULLIF($7,''),NULLIF($8,''),$9::date,$10::jsonb,$11::jsonb,$12,$13,$14)`,
		uuid.NewString(), c.TenantID, cur.LegalEntityID, cur.ProfileID, h.Command, c.PrincipalID, h.Reason, h.Evidence, h.EffectiveFrom,
		jsonb(h.Prior), jsonb(h.New), PolicyRefLimitMax+"="+cur.PolicyMax.String(), c.CorrelationID, h.At.UTC()); err != nil {
		return nil, kit.MapPgErr(err)
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: cur.ProfileID, Command: h.Command,
		ReasonCode: h.Reason, EvidenceID: h.Evidence, After: h.New,
		Detail: map[string]any{"profile_version": 1, "customer_party_id": cur.CustomerPartyID, "effective_from": h.EffectiveFrom,
			"policy_ref": PolicyRefLimitMax + "=" + cur.PolicyMax.String()}}); err != nil {
		return nil, err
	}
	if err := kit.Emit(ctx, tx, c, events(&nh)...); err != nil {
		return nil, err
	}
	// Reload for timestamps.
	return getHeader(ctx, tx, c.TenantID, cur.ProfileID, false)
}

func insertLimitRequest(ctx context.Context, tx pgx.Tx, c kit.Caller, profileID, reqID string, limit kit.Amount, currency string, prior, policyMax kit.Amount, reason, evidence string) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar01_credit_limit_requests (request_id, tenant_id, legal_entity_id, profile_id, requested_limit, currency,
			prior_limit, policy_max_at_request, requested_by, reason_code, evidence_id)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7::numeric,$8::numeric,$9,$10,NULLIF($11,''))`,
		reqID, c.TenantID, c.LegalEntityID, profileID, limit.String(), currency, prior.String(), policyMax.String(), c.PrincipalID, reason, evidence)
	if kit.IsUniqueViolation(err, "uq_ar01_request_pending") {
		return kit.Conflict(CodeLimitPending, "a credit limit change is already awaiting approval for this profile")
	}
	return kit.MapPgErr(err)
}

func limitRequestedEvent(c kit.Caller, h *Header, reqID string, limit kit.Amount, currency string, prior kit.Amount) kit.Event {
	return kit.Event{AggregateType: AggregateType, AggregateID: h.ProfileID, EventType: EventCreditLimitRequested,
		Payload: evPayload(c, h, map[string]any{"request_id": reqID, "requested_limit": limit.String(), "prior_limit": prior.String(),
			"credit_currency": currency, "requested_by": c.PrincipalID})}
}

// ── AmendFinancialProfile ────────────────────────────────────────────────────

type amendReq struct {
	cmdBase
	Transition           string          `json:"transition,omitempty"` // ACTIVATE | SUSPEND | REINSTATE
	InvoicePreferences   *InvoicePrefs   `json:"invoice_preferences,omitempty"`
	StatementPreferences *StatementPrefs `json:"statement_preferences,omitempty"`
	DunningProfile       *string         `json:"dunning_profile,omitempty"`
	ARAccountPolicy      *string         `json:"ar_account_policy,omitempty"`
	RiskAttributes       *RiskAttributes `json:"risk_attributes,omitempty"`
}

func (a amendReq) hasFields() bool {
	return a.InvoicePreferences != nil || a.StatementPreferences != nil || a.DunningProfile != nil || a.ARAccountPolicy != nil || a.RiskAttributes != nil
}

func (m *Module) amendFinancialProfile(w http.ResponseWriter, r *http.Request) {
	var req amendReq
	if !kit.Decode(w, r, &req) {
		return
	}
	var tr Transition
	switch req.Transition {
	case "":
	case "ACTIVATE", "SUSPEND", "REINSTATE":
		tr = Transition(req.Transition)
	default:
		m.fail(w, kit.Invalid("transition must be ACTIVATE, SUSPEND or REINSTATE"))
		return
	}
	if !req.hasFields() && tr == "" {
		m.fail(w, kit.Invalid("nothing to amend: supply attributes and/or a transition"))
		return
	}
	if (tr == TrSuspend || tr == TrReinstate) && !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if req.RiskAttributes != nil {
		if err := req.RiskAttributes.Validate(); err != nil {
			m.fail(w, err)
			return
		}
		if !kit.RequireReason(w, req.ReasonCode) {
			return
		}
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionProfileManage)
	if !ok {
		return
	}
	if tr != TrSuspend { // protective suspension of a terminated customer must stay possible
		if err := m.requireParty(r.Context(), c, h.CustomerPartyID); err != nil {
			m.fail(w, err)
			return
		}
	}
	var out *Profile
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		ctx := r.Context()
		cur, err := lock(ctx, tx, c, &req.cmdBase)
		if err != nil {
			return err
		}
		next := cur.State
		if tr != "" {
			if next, err = Next(cur.State, tr); err != nil {
				return err
			}
		}
		prior, nw := map[string]any{}, map[string]any{}
		eff := ""
		if req.hasFields() {
			if cur.State == StateRetired {
				return kit.Conflict(kit.CodeInvalidTransition, "a retired profile cannot be amended")
			}
			head, err := headAttr(ctx, tx, c.TenantID, cur.ProfileID)
			if err != nil {
				return err
			}
			na := head.Attributes
			if req.InvoicePreferences != nil {
				na.InvoicePreferences = *req.InvoicePreferences
			}
			if req.StatementPreferences != nil {
				na.StatementPreferences = *req.StatementPreferences
			}
			if req.DunningProfile != nil {
				na.DunningProfile = *req.DunningProfile
			}
			if req.ARAccountPolicy != nil {
				na.ARAccountPolicy = *req.ARAccountPolicy
			}
			if req.RiskAttributes != nil {
				na.RiskAttributes = *req.RiskAttributes
			}
			if err := na.Validate(); err != nil {
				return err
			}
			prior, nw = attrDiff(head.Attributes, na)
			if len(nw) == 0 {
				return kit.Invalid("the amendment changes nothing")
			}
			eff = effectiveForChange(m.today(), head)
			if err := insertAttr(ctx, tx, c, cur.ProfileID, head.Seq+1, eff, "AMEND", req.ReasonCode, req.EvidenceID, "", "", na); err != nil {
				return err
			}
		}
		if tr != "" {
			prior["state"], nw["state"] = cur.State, next
		}
		h, err := commit(ctx, tx, c, cur, next, hist{At: m.deps.Now(), Command: "amendFinancialProfile", Reason: req.ReasonCode, Evidence: req.EvidenceID,
			EffectiveFrom: eff, Prior: prior, New: nw}, func(h *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: h.ProfileID, EventType: EventProfileChanged,
				Payload: evPayload(c, h, map[string]any{"changed": nw, "prior": prior, "reason_code": req.ReasonCode, "effective_from": eff})}}
		})
		if err != nil {
			return err
		}
		out, err = m.respond(ctx, tx, c, h)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── SetCreditLimit / approveCreditLimitChange ────────────────────────────────

type setLimitReq struct {
	cmdBase
	CreditLimit    kit.Amount `json:"credit_limit"`
	CreditCurrency string     `json:"credit_currency"`
}

func (m *Module) setCreditLimit(w http.ResponseWriter, r *http.Request) {
	var req setLimitReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if !currencyRe.MatchString(req.CreditCurrency) || req.CreditLimit.IsNegative() || !req.CreditLimit.FitsCurrency(req.CreditCurrency) {
		m.fail(w, kit.Invalid("credit_currency must be an ISO code and credit_limit a non-negative amount that fits it"))
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionLimitManage)
	if !ok {
		return
	}
	if err := m.requireParty(r.Context(), c, h.CustomerPartyID); err != nil {
		m.fail(w, err)
		return
	}
	status := http.StatusOK
	var out *Profile
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		ctx := r.Context()
		cur, err := lock(ctx, tx, c, &req.cmdBase)
		if err != nil {
			return err
		}
		if cur.State == StateRetired {
			return kit.Conflict(kit.CodeInvalidTransition, "a retired profile cannot be changed")
		}
		if p, err := pendingRequest(ctx, tx, c.TenantID, cur.ProfileID, false); err != nil {
			return err
		} else if p != nil {
			return kit.Conflict(CodeLimitPending, "a credit limit change is already awaiting approval for this profile")
		}
		head, err := headAttr(ctx, tx, c.TenantID, cur.ProfileID)
		if err != nil {
			return err
		}
		if head.CreditLimit.IsPositive() && head.CreditCurrency != req.CreditCurrency {
			return kit.Unprocessable(kit.CodeValidationFailed, "credit_currency cannot change while a limit is in force; set the limit to zero first")
		}
		if head.CreditLimit == req.CreditLimit && head.CreditCurrency == req.CreditCurrency {
			return kit.Invalid("the credit limit is unchanged")
		}
		// A decrease, or an increase within the profile's policy ceiling, is
		// immediate; any other increase waits for a different principal.
		immediate := req.CreditLimit <= head.CreditLimit || req.CreditLimit <= cur.PolicyMax
		if !immediate {
			reqID := uuid.NewString()
			if err := insertLimitRequest(ctx, tx, c, cur.ProfileID, reqID, req.CreditLimit, req.CreditCurrency, head.CreditLimit, cur.PolicyMax, req.ReasonCode, req.EvidenceID); err != nil {
				return err
			}
			h, err := commit(ctx, tx, c, cur, cur.State, hist{At: m.deps.Now(), Command: "setCreditLimit.requested", Reason: req.ReasonCode, Evidence: req.EvidenceID,
				Prior: map[string]any{"credit_limit": head.CreditLimit.String()},
				New:   map[string]any{"requested_limit": req.CreditLimit.String(), "request_id": reqID, "status": "PENDING"}},
				func(h *Header) []kit.Event {
					return []kit.Event{limitRequestedEvent(c, h, reqID, req.CreditLimit, req.CreditCurrency, head.CreditLimit)}
				})
			if err != nil {
				return err
			}
			status = http.StatusAccepted
			out, err = m.respond(ctx, tx, c, h)
			return err
		}
		na := head.Attributes
		na.CreditLimit, na.CreditCurrency = req.CreditLimit, req.CreditCurrency
		eff := effectiveForChange(m.today(), head)
		if err := insertAttr(ctx, tx, c, cur.ProfileID, head.Seq+1, eff, "CREDIT_LIMIT", req.ReasonCode, req.EvidenceID, "", "", na); err != nil {
			return err
		}
		h, err := commit(ctx, tx, c, cur, cur.State, hist{At: m.deps.Now(), Command: "setCreditLimit", Reason: req.ReasonCode, Evidence: req.EvidenceID, EffectiveFrom: eff,
			Prior: map[string]any{"credit_limit": head.CreditLimit.String(), "credit_currency": head.CreditCurrency},
			New:   map[string]any{"credit_limit": req.CreditLimit.String(), "credit_currency": req.CreditCurrency}},
			func(h *Header) []kit.Event {
				return []kit.Event{limitChangedEvent(c, h, head.CreditLimit, req.CreditLimit, req.CreditCurrency, eff, "")}
			})
		if err != nil {
			return err
		}
		out, err = m.respond(ctx, tx, c, h)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, status, out)
}

func limitChangedEvent(c kit.Caller, h *Header, prior, nw kit.Amount, currency, eff, approver string) kit.Event {
	extra := map[string]any{"prior_limit": prior.String(), "credit_limit": nw.String(), "credit_currency": currency, "effective_from": eff, "changed_by": c.PrincipalID}
	if approver != "" {
		extra["approved_by"] = approver
	}
	return kit.Event{AggregateType: AggregateType, AggregateID: h.ProfileID, EventType: EventCreditLimitChanged, Payload: evPayload(c, h, extra)}
}

type approveReq struct {
	cmdBase
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"` // APPROVE | REJECT
}

// approveCreditLimitChange is the maker-checker step for an out-of-policy limit
// increase: it needs ActionLimitApprove and a principal different from the requester.
func (m *Module) approveCreditLimitChange(w http.ResponseWriter, r *http.Request) {
	var req approveReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if err := mustUUID("request_id", req.RequestID); err != nil {
		m.fail(w, err)
		return
	}
	if req.Decision != "APPROVE" && req.Decision != "REJECT" {
		m.fail(w, kit.Invalid("decision must be APPROVE or REJECT"))
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionLimitApprove)
	if !ok {
		return
	}
	if req.Decision == "APPROVE" {
		if err := m.requireParty(r.Context(), c, h.CustomerPartyID); err != nil {
			m.fail(w, err)
			return
		}
	}
	var out *Profile
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		ctx := r.Context()
		cur, err := lock(ctx, tx, c, &req.cmdBase)
		if err != nil {
			return err
		}
		if cur.State == StateRetired {
			return kit.Conflict(kit.CodeInvalidTransition, "a retired profile cannot be changed")
		}
		pr, err := pendingRequest(ctx, tx, c.TenantID, cur.ProfileID, true)
		if err != nil {
			return err
		}
		if pr == nil || pr.RequestID != req.RequestID {
			return kit.NotFound("no such pending credit limit request on this profile")
		}
		if req.Decision == "APPROVE" && pr.RequestedBy == c.PrincipalID {
			return errSoD
		}
		if _, err := tx.Exec(ctx, `UPDATE ar01_credit_limit_requests SET status = $3, decided_by = $4, decided_at = now(), decision_reason = $5
			WHERE tenant_id = $1 AND request_id = $2 AND status = 'PENDING'`, c.TenantID, pr.RequestID,
			map[string]string{"APPROVE": "APPROVED", "REJECT": "REJECTED"}[req.Decision], c.PrincipalID, req.ReasonCode); err != nil {
			return kit.MapPgErr(err)
		}
		if req.Decision == "REJECT" {
			h, err := commit(ctx, tx, c, cur, cur.State, hist{At: m.deps.Now(), Command: "approveCreditLimitChange.rejected", Reason: req.ReasonCode, Evidence: req.EvidenceID,
				ApproverID: c.PrincipalID, Prior: map[string]any{"requested_limit": pr.RequestedLimit.String(), "status": "PENDING"},
				New: map[string]any{"status": "REJECTED", "request_id": pr.RequestID, "requested_by": pr.RequestedBy}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.ProfileID, EventType: EventCreditLimitRejected,
						Payload: evPayload(c, h, map[string]any{"request_id": pr.RequestID, "requested_limit": pr.RequestedLimit.String(), "rejected_by": c.PrincipalID})}}
				})
			if err != nil {
				return err
			}
			out, err = m.respond(ctx, tx, c, h)
			return err
		}
		head, err := headAttr(ctx, tx, c.TenantID, cur.ProfileID)
		if err != nil {
			return err
		}
		na := head.Attributes
		na.CreditLimit, na.CreditCurrency = pr.RequestedLimit, pr.Currency
		eff := effectiveForChange(m.today(), head)
		// The version records the requester as the maker and the caller as the approver.
		mk := c
		mk.PrincipalID = pr.RequestedBy
		if err := insertAttr(ctx, tx, mk, cur.ProfileID, head.Seq+1, eff, "CREDIT_LIMIT", pr.ReasonCode, pr.EvidenceID, c.PrincipalID, pr.RequestID, na); err != nil {
			return err
		}
		h, err := commit(ctx, tx, c, cur, cur.State, hist{At: m.deps.Now(), Command: "approveCreditLimitChange", Reason: req.ReasonCode, Evidence: req.EvidenceID,
			ApproverID: c.PrincipalID, EffectiveFrom: eff,
			Prior: map[string]any{"credit_limit": head.CreditLimit.String(), "credit_currency": head.CreditCurrency},
			New: map[string]any{"credit_limit": pr.RequestedLimit.String(), "credit_currency": pr.Currency, "request_id": pr.RequestID,
				"requested_by": pr.RequestedBy, "request_reason_code": pr.ReasonCode}},
			func(h *Header) []kit.Event {
				ev := limitChangedEvent(c, h, head.CreditLimit, pr.RequestedLimit, pr.Currency, eff, c.PrincipalID)
				ev.Payload.(map[string]any)["changed_by"] = pr.RequestedBy
				return []kit.Event{ev}
			})
		if err != nil {
			return err
		}
		out, err = m.respond(ctx, tx, c, h)
		return err
	})
	if err != nil {
		if err == errSoD {
			m.recordSoDDenial(r.Context(), c, h, &req)
		}
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

var errSoD = kit.Forbidden(kit.CodeSegregationOfDuties, "the principal who requested a credit limit increase cannot approve it")

// recordSoDDenial keeps evidence of the refused self-approval (the command's own
// transaction rolled back, so the audit is written separately).
func (m *Module) recordSoDDenial(ctx context.Context, c kit.Caller, h *Header, req *approveReq) {
	_ = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: h.ProfileID, Command: "approveCreditLimitChange.denied",
			ReasonCode: req.ReasonCode, EvidenceID: req.EvidenceID,
			Detail: map[string]any{"denial": kit.CodeSegregationOfDuties, "request_id": req.RequestID, "decision": req.Decision}})
	})
}

// ── PlaceCreditHold / ReleaseCreditHold / RetireFinancialProfile ─────────────

// Decision (AR-01 SoD "high-risk profile changes may require maker-checker"):
// a credit hold may be released by any principal holding AR01_CREDIT_HOLD_MANAGE,
// including the one who placed it. A hold is a protective control, not a
// value-increasing change; the placer, releaser, reason and evidence are all
// recorded, and the out-of-policy limit increase is the maker-checker case.

func (m *Module) placeCreditHold(w http.ResponseWriter, r *http.Request) {
	m.stateCommand(w, r, ActionHoldManage, TrHold, "placeCreditHold", EventCreditHoldPlaced, true, false)
}

func (m *Module) releaseCreditHold(w http.ResponseWriter, r *http.Request) {
	m.stateCommand(w, r, ActionHoldManage, TrRelease, "releaseCreditHold", EventCreditHoldReleased, true, true)
}

func (m *Module) retireFinancialProfile(w http.ResponseWriter, r *http.Request) {
	m.stateCommand(w, r, ActionProfileManage, TrRetire, "retireFinancialProfile", EventProfileChanged, true, false)
}

func (m *Module) stateCommand(w http.ResponseWriter, r *http.Request, action string, tr Transition, command, eventType string, needReason, needParty bool) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	if needReason && !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	c, h, ok := m.prepare(w, r, &req, action)
	if !ok {
		return
	}
	if needParty {
		if err := m.requireParty(r.Context(), c, h.CustomerPartyID); err != nil {
			m.fail(w, err)
			return
		}
	}
	var out *Profile
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		ctx := r.Context()
		cur, err := lock(ctx, tx, c, &req)
		if err != nil {
			return err
		}
		next, err := Next(cur.State, tr)
		if err != nil {
			return err
		}
		prior := map[string]any{"state": cur.State, "credit_hold": cur.CreditHold}
		nw := map[string]any{"state": next, "credit_hold": next == StateOnHold}
		if tr == TrRelease {
			prior["hold_reason_code"], prior["hold_placed_by"] = cur.HoldReasonCode, cur.HoldPlacedBy
		}
		h, err := commit(ctx, tx, c, cur, next, hist{At: m.deps.Now(), Command: command, Reason: req.ReasonCode, Evidence: req.EvidenceID, Prior: prior, New: nw},
			func(h *Header) []kit.Event {
				extra := map[string]any{"reason_code": req.ReasonCode, "evidence_id": req.EvidenceID, "prior_state": cur.State}
				if tr == TrRelease {
					extra["placed_by"], extra["released_by"] = cur.HoldPlacedBy, c.PrincipalID
				}
				if tr == TrHold {
					extra["placed_by"] = c.PrincipalID
				}
				return []kit.Event{{AggregateType: AggregateType, AggregateID: h.ProfileID, EventType: eventType, Payload: evPayload(c, h, extra)}}
			})
		if err != nil {
			return err
		}
		out, err = m.respond(ctx, tx, c, h)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── ChangePaymentTerms ───────────────────────────────────────────────────────

type termsReq struct {
	cmdBase
	PaymentTermsCode string `json:"payment_terms_code"`
	PaymentTermsDays int    `json:"payment_terms_days"`
	EffectiveFrom    string `json:"effective_from,omitempty"`
}

func (m *Module) changePaymentTerms(w http.ResponseWriter, r *http.Request) {
	var req termsReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if err := ValidateTerms(req.PaymentTermsCode, req.PaymentTermsDays); err != nil {
		m.fail(w, err)
		return
	}
	today := m.today()
	eff := req.EffectiveFrom
	if eff == "" {
		eff = today
	} else if _, err := ParseDay(eff); err != nil {
		m.fail(w, err)
		return
	} else if eff < today {
		m.fail(w, kit.Invalid("effective_from must not be in the past: payment terms history is not rewritten"))
		return
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionProfileManage)
	if !ok {
		return
	}
	if err := m.requireParty(r.Context(), c, h.CustomerPartyID); err != nil {
		m.fail(w, err)
		return
	}
	var out *Profile
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		ctx := r.Context()
		cur, err := lock(ctx, tx, c, &req.cmdBase)
		if err != nil {
			return err
		}
		if cur.State == StateRetired {
			return kit.Conflict(kit.CodeInvalidTransition, "a retired profile cannot be changed")
		}
		last, err := latestTerm(ctx, tx, c.TenantID, cur.ProfileID)
		if err != nil {
			return err
		}
		if last.Code == req.PaymentTermsCode && last.Days == req.PaymentTermsDays && last.EffectiveTo == "" {
			return kit.Invalid("the payment terms are unchanged")
		}
		// Overlap rule: the new interval must start strictly after the latest
		// version starts, so exactly one version is ever effective on a date.
		if eff <= last.EffectiveFrom {
			return errOverlap("effective_from " + eff + " does not follow the latest payment terms version (effective from " + last.EffectiveFrom + ")")
		}
		if last.EffectiveTo == "" {
			if err := closeTerm(ctx, tx, c.TenantID, cur.ProfileID, last.Seq, eff); err != nil {
				return err
			}
		}
		if err := insertTerm(ctx, tx, c, cur.ProfileID, last.Seq+1, req.PaymentTermsCode, req.PaymentTermsDays, eff, req.ReasonCode, req.EvidenceID); err != nil {
			return err
		}
		prior := map[string]any{"payment_terms_code": last.Code, "payment_terms_days": last.Days, "effective_from": last.EffectiveFrom, "effective_to": eff}
		nw := map[string]any{"payment_terms_code": req.PaymentTermsCode, "payment_terms_days": req.PaymentTermsDays, "effective_from": eff}
		h, err := commit(ctx, tx, c, cur, cur.State, hist{At: m.deps.Now(), Command: "changePaymentTerms", Reason: req.ReasonCode, Evidence: req.EvidenceID,
			EffectiveFrom: eff, Prior: prior, New: nw}, func(h *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: h.ProfileID, EventType: EventProfileChanged,
				Payload: evPayload(c, h, map[string]any{"changed": nw, "prior": prior, "reason_code": req.ReasonCode, "effective_from": eff})}}
		})
		if err != nil {
			return err
		}
		out, err = m.respond(ctx, tx, c, h)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}
