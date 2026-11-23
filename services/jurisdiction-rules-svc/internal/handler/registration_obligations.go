package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 s15 and s16: registration-driven applicability. Given the registrations an entity holds, which
// obligations apply and when is each due?
//
// The registration TYPE rule names the obligation codes it brings (registration.obligations); each obligation's due
// date is derived by the same engine as POST /v1/obligation-calculations:calculate, with the registration date supplied
// as the registration_date fact. The registrations themselves are tenant facts owned by tenant-entity-registry-svc: the
// caller sends their type code and dates, never a registration number, and this service stores none of them.
//
// Per-entity obligation INSTANCES (status, overdue detection, filing) are owned by obligations-svc (s27: "Obligation
// Service: entity-specific obligation"), which already publishes obligation.created and obligation.overdue. This endpoint
// answers what applies and when it is due; it does not track anything.

const maxRegistrationFacts = 25

type RegistrationFact struct {
	// RegistrationCode is the registration type rule code (REGISTRATION_TYPE domain), for example VAT_NUMBER.
	RegistrationCode string `json:"registration_code"`
	// RegistrationDate is the day the registration took effect (YYYY-MM-DD).
	RegistrationDate string `json:"registration_date"`
	// DeregistrationDate, when given, is the first day the registration no longer applies.
	DeregistrationDate *string `json:"deregistration_date"`
}

type RegistrationObligationsRequest struct {
	Jurisdiction  string             `json:"jurisdiction"`
	AsOf          string             `json:"as_of"`
	Registrations []RegistrationFact `json:"registrations"`
	// Facts are the period facts the obligations anchor on (period_end, event_date, anniversary_date and anchor_year, extension).
	// registration_date is set per registration and is ignored here.
	Facts       domain.DueFacts `json:"facts"`
	PackRef     string          `json:"pack_ref"`
	PackVersion string          `json:"pack_version"`
}

// ObligationApplicability is one obligation a registration brings, with its derived due date or the reason there is none.
type ObligationApplicability struct {
	ObligationCode string `json:"obligation_code"`
	Outcome        string `json:"outcome"`
	// Reason says why no due date was derived when the cause is the caller's facts, not a missing rule.
	Reason  string                `json:"reason,omitempty"`
	DueDate string                `json:"due_date,omitempty"`
	Due     *resolver.DueDecision `json:"due,omitempty"`
}

// RegistrationApplicability is the answer for one registration.
type RegistrationApplicability struct {
	RegistrationCode string                    `json:"registration_code"`
	Kind             string                    `json:"kind,omitempty"`
	RegistrationDate string                    `json:"registration_date"`
	InForce          bool                      `json:"in_force"`
	Outcome          string                    `json:"outcome"`
	Reason           string                    `json:"reason,omitempty"`
	Obligations      []ObligationApplicability `json:"obligations"`
}

// RegistrationObligationsResult is the explainable outcome.
type RegistrationObligationsResult struct {
	Outcome       string                      `json:"outcome"`
	AsOf          string                      `json:"as_of"`
	Registrations []RegistrationApplicability `json:"registrations"`
	Explanation   []string                    `json:"explanation"`
}

const (
	OutcomeApplicabilityResolved = "APPLICABILITY_RESOLVED"
	registrationNotInForce       = "NOT_IN_FORCE"
	registrationApplies          = "APPLIES"
)

// ResolveRegistrationObligations lists the obligations that apply for an entity's registrations and derives each due date.
//
//	200 APPLICABILITY_RESOLVED (per-registration and per-obligation outcomes are inside: NOT_IN_FORCE, NO_RULE, NO_OBLIGATION_RULE, INVALID_FACTS ...)
//	400 invalid request      409 idempotency_conflict      503 pack_unverified (fail closed)
func (h *Handler) ResolveRegistrationObligations(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "registration_obligation", "resolve")
	if !ok {
		return
	}
	var req RegistrationObligationsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"as_of", req.AsOf}); field != "" {
		writeMissingField(w, r, field)
		return
	}
	asOf, ok := parseDay(w, r, "as_of", req.AsOf)
	if !ok {
		return
	}
	if len(req.Registrations) == 0 || len(req.Registrations) > maxRegistrationFacts {
		writeError(w, r, http.StatusBadRequest, "invalid_registrations", "registrations needs 1 to 25 entries")
		return
	}
	type parsed struct {
		from time.Time
		to   *time.Time
	}
	dates := make([]parsed, len(req.Registrations))
	for i, rf := range req.Registrations {
		if field := firstBlank(requiredField{"registration_code", rf.RegistrationCode}, requiredField{"registration_date", rf.RegistrationDate}); field != "" {
			writeMissingField(w, r, "registrations[]."+field)
			return
		}
		from, ok := parseDay(w, r, "registration_date", rf.RegistrationDate)
		if !ok {
			return
		}
		dates[i].from = from
		if rf.DeregistrationDate != nil {
			to, ok := parseDay(w, r, "deregistration_date", *rf.DeregistrationDate)
			if !ok {
				return
			}
			if !to.After(from) {
				writeError(w, r, http.StatusBadRequest, "invalid_deregistration_date", "deregistration_date must be after registration_date")
				return
			}
			dates[i].to = &to
		}
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	rawReq, _ := json.Marshal(req)
	reqDigest, _ := domain.DigestOf(rawReq)
	corr := r.Header.Get("X-Correlation-ID")

	res := RegistrationObligationsResult{Outcome: OutcomeApplicabilityResolved, AsOf: asOf.Format("2006-01-02"),
		Registrations: []RegistrationApplicability{}, Explanation: []string{}}
	for i, rf := range req.Registrations {
		item := RegistrationApplicability{RegistrationCode: rf.RegistrationCode, RegistrationDate: dates[i].from.Format("2006-01-02"), Obligations: []ObligationApplicability{}}
		item.InForce = !dates[i].from.After(asOf) && (dates[i].to == nil || asOf.Before(*dates[i].to))
		if !item.InForce {
			item.Outcome = registrationNotInForce
			item.Reason = "the registration is not in force on as_of: it starts after it or ended before it"
			res.Registrations = append(res.Registrations, item)
			continue
		}
		d, p, outcome, ok := h.resolveRegulatedRule(w, r, regLookup{jurisdiction: req.Jurisdiction, ruleDomain: domain.RegistrationTypeDomain, code: rf.RegistrationCode,
			family: domain.FamilyRegistrationType, packRef: req.PackRef, packVersion: req.PackVersion, at: asOf})
		if !ok {
			return
		}
		_ = d
		if p == nil {
			item.Outcome = outcome
			item.Reason = "no registration type rule answers for this code: nothing is assumed about the obligations it would bring"
			res.Registrations = append(res.Registrations, item)
			continue
		}
		item.Kind, item.Outcome = p.Registration.Kind, registrationApplies
		for _, code := range p.Registration.Obligations {
			facts := req.Facts
			facts.RegistrationDate = dates[i].from.Format("2006-01-02")
			dd, err := h.resolver.CalculateDue(r.Context(), resolver.DueRequest{Jurisdiction: req.Jurisdiction, ObligationCode: code, Facts: facts,
				PackRef: req.PackRef, PackVersion: req.PackVersion})
			if err != nil {
				h.writeResolveError(w, r, err, corr)
				return
			}
			ob := ObligationApplicability{ObligationCode: code, Outcome: dd.Outcome, Due: dd}
			if dd.Due != nil {
				ob.DueDate = dd.Due.DueDate
			}
			if ob.DueDate == "" && (dd.Outcome == domain.DueNoRule || dd.Outcome == domain.DueInvalidFacts) && req.Facts.PeriodEnd == "" && req.Facts.EventDate == "" && req.Facts.AnniversaryDate == "" {
				ob.Reason = "no period fact (period_end, event_date or anniversary_date) was supplied: an obligation anchored on one cannot be derived from the registration date alone"
			}
			item.Obligations = append(item.Obligations, ob)
		}
		res.Registrations = append(res.Registrations, item)
	}
	res.Explanation = append(res.Explanation, "applicability follows the registration type rules in force on as_of; due dates are derived, never entered. "+
		"Tracking each obligation (status, overdue, filing) belongs to obligations-svc")

	respJSON, err := json.Marshal(res)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "encode_failed", "could not encode the answer")
		return
	}
	stored, replayed, err := h.registry.RecordDecision(r.Context(), store.DecisionRecord{Kind: "APPLICABILITY", RequestedBy: actor, CorrelationID: corr,
		IdempotencyKey: idemKey, RequestDigest: reqDigest, RequestJSON: rawReq, EffectiveAt: asOf, Outcome: res.Outcome, ResponseJSON: respJSON})
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyConflict) {
			writeError(w, r, http.StatusConflict, "idempotency_conflict", err.Error())
			return
		}
		h.writeRegistryError(w, r, err, corr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decision_id": stored.DecisionID, "replayed": replayed, "applicability": json.RawMessage(stored.Response)})
}
