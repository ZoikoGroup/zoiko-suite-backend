package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 4 (calendar half): regulatory calendars, obligation rules
// and the due-date calculation API (s15, s16, s25).
//
// Calendar versions and obligation rules are authored as DRAFT, given source
// provenance, and PUBLISHED by a different principal; PUBLISHED is immutable,
// and an amendment is a new version. They reach runtime only inside signed,
// certified, released packs, never from these tables.

func registerCalendarRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/calendars", h.ListCalendars)
	r.Get("/v1/calendars/{calendar_code}", h.GetCalendar)
	r.Get("/v1/calendars/{calendar_code}/versions", h.ListCalendarVersions)
	r.Get("/v1/calendars/{calendar_code}/versions/{version}", h.GetCalendarVersion)
	r.Get("/v1/obligation-rules", h.ListObligationRules)
	r.Get("/v1/obligation-rules/{obligation_rule_id}", h.GetObligationRule)

	r.Post("/v1/admin/calendars", h.CreateCalendar)
	r.Post("/v1/admin/calendars/{calendar_code}/versions", h.CreateCalendarVersion)
	r.Put("/v1/admin/calendars/{calendar_code}/versions/{version}/sources", h.SetCalendarVersionSources)
	r.Post("/v1/admin/calendars/{calendar_code}/versions/{version}/publish", h.PublishCalendarVersion)
	r.Post("/v1/admin/obligation-rules", h.CreateObligationRule)
	r.Put("/v1/admin/obligation-rules/{obligation_rule_id}/provenance", h.SetObligationProvenance)
	r.Post("/v1/admin/obligation-rules/{obligation_rule_id}/publish", h.PublishObligationRule)
}

func registerObligationCalcRoute(r chi.Router, h *Handler) {
	r.Post("/v1/obligation-calculations:calculate", h.CalculateObligation)
}

func (h *Handler) writeCalendarError(w http.ResponseWriter, err error, corr string) {
	switch {
	case errors.Is(err, domain.ErrObligationInvalid):
		writeError(w, http.StatusBadRequest, "invalid_definition", err.Error())
	case errors.Is(err, domain.ErrCalendarNotFound):
		writeError(w, http.StatusNotFound, "calendar_not_found", err.Error())
	case errors.Is(err, domain.ErrObligationRuleNotFound):
		writeError(w, http.StatusNotFound, "obligation_rule_not_found", err.Error())
	case errors.Is(err, domain.ErrNotIndependent):
		writeError(w, http.StatusForbidden, "segregation_of_duties", err.Error())
	default:
		h.writeOpsError(w, err, corr)
	}
}

// ── calendars ───────────────────────────────────────────────────────────────

type CreateCalendarRequest struct {
	CalendarCode   string  `json:"calendar_code"`
	JurisdictionID string  `json:"jurisdiction_id"`
	Authority      string  `json:"authority"`
	Description    *string `json:"description"`
}

func (h *Handler) CreateCalendar(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_calendar", "create")
	if !ok {
		return
	}
	var req CreateCalendarRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !domain.ValidCalendarCode(req.CalendarCode) {
		writeError(w, http.StatusBadRequest, "invalid_calendar_code", "calendar_code must be lowercase letters, digits and . _ - (2-64 characters)")
		return
	}
	if field := firstBlank(requiredField{"jurisdiction_id", req.JurisdictionID}, requiredField{"authority", req.Authority}); field != "" {
		writeMissingField(w, field)
		return
	}
	c, created, err := h.registry.CreateCalendar(r.Context(), req.CalendarCode, req.JurisdictionID, req.Authority, req.Description, actor)
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		writeJSON(w, http.StatusCreated, c)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListCalendars(w http.ResponseWriter, r *http.Request) {
	cs, err := h.registry.ListCalendars(r.Context())
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calendars": cs})
}

func (h *Handler) GetCalendar(w http.ResponseWriter, r *http.Request) {
	c, err := h.registry.GetCalendar(r.Context(), chi.URLParam(r, "calendar_code"))
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type CreateCalendarVersionRequest struct {
	EffectiveFrom string           `json:"effective_from"`
	Timezone      string           `json:"timezone"`
	WeekendDays   *[]int           `json:"weekend_days"`
	CutoffTime    *string          `json:"cutoff_time"`
	Holidays      []domain.Holiday `json:"holidays"`
}

// CreateCalendarVersion drafts the next version. The weekend definition is
// required, not defaulted: it differs between authorities.
func (h *Handler) CreateCalendarVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_calendar_version", "create")
	if !ok {
		return
	}
	var req CreateCalendarVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"effective_from", req.EffectiveFrom}, requiredField{"timezone", req.Timezone}); field != "" {
		writeMissingField(w, field)
		return
	}
	if req.WeekendDays == nil {
		writeMissingField(w, "weekend_days")
		return
	}
	v, err := h.registry.CreateCalendarVersion(r.Context(), chi.URLParam(r, "calendar_code"), req.EffectiveFrom, req.Timezone, *req.WeekendDays, req.CutoffTime, req.Holidays, actor)
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func parseVersionParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "invalid_version", "version must be a positive integer")
		return 0, false
	}
	return n, true
}

func (h *Handler) GetCalendarVersion(w http.ResponseWriter, r *http.Request) {
	n, ok := parseVersionParam(w, r)
	if !ok {
		return
	}
	v, err := h.registry.GetCalendarVersion(r.Context(), chi.URLParam(r, "calendar_code"), n)
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) ListCalendarVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := h.registry.ListCalendarVersions(r.Context(), chi.URLParam(r, "calendar_code"))
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

type SourceIDsRequest struct {
	SourceIDs []string `json:"source_ids"`
}

// SetCalendarVersionSources replaces the sources of a DRAFT calendar version.
func (h *Handler) SetCalendarVersionSources(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "regulatory_calendar_version", "set_sources"); !ok {
		return
	}
	n, ok := parseVersionParam(w, r)
	if !ok {
		return
	}
	var req SourceIDsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	v, err := h.registry.SetCalendarVersionSources(r.Context(), chi.URLParam(r, "calendar_code"), n, req.SourceIDs)
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// PublishCalendarVersion freezes a calendar version. The publisher must not be
// its author, and it must cite reviewed, non-superseded sources.
func (h *Handler) PublishCalendarVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_calendar_version", "publish")
	if !ok {
		return
	}
	n, ok := parseVersionParam(w, r)
	if !ok {
		return
	}
	code := chi.URLParam(r, "calendar_code")
	v, changed, err := h.registry.PublishCalendarVersion(r.Context(), code, n, actor)
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		// Named in the service's original specification (03-microservices.md s8.2).
		h.emitRegistry(r, events.EventCalendarChanged, v.CalendarVersionID, actor, map[string]any{
			"calendar_code": code, "version": v.Version, "calendar_version_id": v.CalendarVersionID, "effective_from": v.EffectiveFrom})
	}
	writeJSON(w, http.StatusOK, v)
}

// ── obligation rules ────────────────────────────────────────────────────────

type CreateObligationRuleRequest struct {
	JurisdictionID            string  `json:"jurisdiction_id"`
	ObligationCode            string  `json:"obligation_code"`
	Name                      string  `json:"name"`
	PeriodBasis               string  `json:"period_basis"`
	Anchor                    string  `json:"anchor"`
	OffsetMonths              int     `json:"offset_months"`
	OffsetDays                int     `json:"offset_days"`
	OffsetToMonthEnd          bool    `json:"offset_to_month_end"`
	BusinessDayAdjustment     string  `json:"business_day_adjustment"`
	CalendarCode              string  `json:"calendar_code"`
	EffectiveFrom             string  `json:"effective_from"`
	EffectiveTo               *string `json:"effective_to"`
	ExtensionAllowed          bool    `json:"extension_allowed"`
	MaxExtensionDays          int     `json:"max_extension_days"`
	ExtensionRequiresEvidence bool    `json:"extension_requires_evidence"`
	CutoffApplies             bool    `json:"cutoff_applies"`
	EscalationOwner           *string `json:"escalation_owner"`
	EscalationSLAHours        *int    `json:"escalation_sla_hours"`
}

// CreateObligationRule drafts the next version of an obligation rule.
func (h *Handler) CreateObligationRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "obligation_rule", "create")
	if !ok {
		return
	}
	var req CreateObligationRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction_id", req.JurisdictionID}, requiredField{"obligation_code", req.ObligationCode},
		requiredField{"name", req.Name}, requiredField{"period_basis", req.PeriodBasis}, requiredField{"anchor", req.Anchor},
		requiredField{"effective_from", req.EffectiveFrom}); field != "" {
		writeMissingField(w, field)
		return
	}
	adj := strings.ToUpper(strings.TrimSpace(req.BusinessDayAdjustment))
	if adj == "" {
		adj = domain.AdjustNone
	}
	rec, err := h.registry.CreateObligationRule(r.Context(), store.CreateObligationParams{
		JurisdictionID: req.JurisdictionID, ObligationCode: req.ObligationCode, Name: req.Name,
		PeriodBasis: strings.ToUpper(req.PeriodBasis), Anchor: strings.ToUpper(req.Anchor), BusinessDayAdjustment: adj,
		OffsetMonths: req.OffsetMonths, OffsetDays: req.OffsetDays, OffsetToMonthEnd: req.OffsetToMonthEnd, CalendarCode: req.CalendarCode,
		EffectiveFrom: req.EffectiveFrom, EffectiveTo: req.EffectiveTo, ExtensionAllowed: req.ExtensionAllowed, MaxExtensionDays: req.MaxExtensionDays,
		ExtensionRequiresEvidence: req.ExtensionRequiresEvidence, CutoffApplies: req.CutoffApplies, EscalationOwner: req.EscalationOwner,
		EscalationSLAHours: req.EscalationSLAHours, CreatedBy: actor})
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (h *Handler) GetObligationRule(w http.ResponseWriter, r *http.Request) {
	rec, err := h.registry.GetObligationRule(r.Context(), chi.URLParam(r, "obligation_rule_id"))
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) ListObligationRules(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset, ok := h.parsePaging(w, q)
	if !ok {
		return
	}
	rs, err := h.registry.ListObligationRules(r.Context(), q.Get("jurisdiction_id"), q.Get("obligation_code"), limit, offset)
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"obligation_rules": rs})
}

type ObligationProvenanceRequest struct {
	RegimeID         *string  `json:"regime_id"`
	InterpretationID *string  `json:"interpretation_id"`
	SourceIDs        []string `json:"source_ids"`
}

// SetObligationProvenance replaces the provenance of a DRAFT obligation rule (PUT).
func (h *Handler) SetObligationProvenance(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "obligation_rule", "set_provenance"); !ok {
		return
	}
	var req ObligationProvenanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rec, err := h.registry.SetObligationProvenance(r.Context(), chi.URLParam(r, "obligation_rule_id"), req.RegimeID, req.InterpretationID, req.SourceIDs)
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// PublishObligationRule freezes an obligation rule; the publisher must not be its author.
func (h *Handler) PublishObligationRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "obligation_rule", "publish")
	if !ok {
		return
	}
	id := chi.URLParam(r, "obligation_rule_id")
	rec, changed, err := h.registry.PublishObligationRule(r.Context(), id, actor)
	if err != nil {
		h.writeCalendarError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventObligationRulePublished, id, actor, map[string]any{
			"obligation_rule_id": id, "obligation_code": rec.ObligationCode, "rule_version": rec.RuleVersion, "jurisdiction_id": rec.JurisdictionID})
	}
	writeJSON(w, http.StatusOK, rec)
}

// ── calculation ─────────────────────────────────────────────────────────────

// CalculateObligationRequest carries FACTS only. The server selects the pack,
// the obligation rule version and the calendar version.
type CalculateObligationRequest struct {
	Jurisdiction         string `json:"jurisdiction"`
	ObligationCode       string `json:"obligation_code"`
	PeriodEnd            string `json:"period_end"`
	EventDate            string `json:"event_date"`
	RegistrationDate     string `json:"registration_date"`
	AnniversaryDate      string `json:"anniversary_date"`
	AnchorYear           int    `json:"anchor_year"`
	ExtensionDays        int    `json:"extension_days"`
	ExtensionEvidenceRef string `json:"extension_evidence_ref"`
	// AsOf, when given, adds an `overdue` verdict against the calculated due instant.
	AsOf        string `json:"as_of"`
	PackRef     string `json:"pack_ref"`
	PackVersion string `json:"pack_version"`
}

// CalculateObligation derives an obligation's due date from verified, released
// pack artifacts and records the calculation as evidence.
//
//	200 DUE_DATE_CALCULATED or NO_OBLIGATION_RULE
//	409 AMBIGUOUS or NO_CALENDAR      422 UNSUPPORTED_JURISDICTION, INVALID_FACTS, EXTENSION_NOT_PERMITTED
//	503 pack_unverified (fail closed)
func (h *Handler) CalculateObligation(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "obligation_calculation", "calculate")
	if !ok {
		return
	}
	var req CalculateObligationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"obligation_code", req.ObligationCode}); field != "" {
		writeMissingField(w, field)
		return
	}
	var asOf *time.Time
	if req.AsOf != "" {
		t, err := time.Parse(time.RFC3339Nano, req.AsOf)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_as_of", "as_of must be an RFC3339 instant")
			return
		}
		asOf = &t
	}
	var idemKey *string
	if k := strings.TrimSpace(r.Header.Get("Idempotency-Key")); k != "" {
		if len(k) > maxIdempotencyKey {
			writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is too long")
			return
		}
		idemKey = &k
	}
	facts := domain.DueFacts{PeriodEnd: req.PeriodEnd, EventDate: req.EventDate, RegistrationDate: req.RegistrationDate,
		AnniversaryDate: req.AnniversaryDate, AnchorYear: req.AnchorYear, ExtensionDays: req.ExtensionDays, ExtensionEvidence: req.ExtensionEvidenceRef}

	// as_of is deliberately NOT part of the digest: it only asks for an overdue
	// verdict and never changes the calculation, so a retry with a different
	// as_of is the same question and returns the stored calculation.
	canon := req
	canon.AsOf = ""
	rawReq, _ := json.Marshal(canon)
	reqDigest, _ := domain.DigestOf(rawReq)

	corr := r.Header.Get("X-Correlation-ID")
	d, err := h.resolver.CalculateDue(r.Context(), resolver.DueRequest{Jurisdiction: req.Jurisdiction, ObligationCode: req.ObligationCode,
		Facts: facts, PackRef: req.PackRef, PackVersion: req.PackVersion})
	if err != nil {
		h.writeResolveError(w, err, corr)
		return
	}
	respJSON, err := json.Marshal(d)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed", "could not encode the calculation")
		return
	}
	sel, _ := domain.SelectionDate(facts)
	rec := store.DecisionRecord{Kind: "OBLIGATION", RequestedBy: actor, CorrelationID: corr, IdempotencyKey: idemKey, RequestDigest: reqDigest,
		RequestJSON: rawReq, EffectiveAt: sel.UTC(), Outcome: d.Outcome, ResponseJSON: respJSON}
	rec.ResolverRing, rec.ResolverRegion = h.resolver.Scope()
	if d.Pack != nil {
		rec.PackRef, rec.PackVersion, rec.PackVersionID = &d.Pack.PackRef, &d.Pack.Version, &d.Pack.PackVersionID
		rec.ArtifactDigest = &d.Pack.ArtifactDigest
		if d.Pack.CertificationID != "" {
			rec.CertificationID = &d.Pack.CertificationID
		}
	}
	if d.Due != nil && d.Due.ObligationRuleID != "" {
		rec.RuleID, rec.RuleContentDigest = &d.Due.ObligationRuleID, &d.Due.ContentDigest
	}
	stored, replayed, err := h.registry.RecordDecision(r.Context(), rec)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
			return
		}
		h.writeRegistryError(w, err, corr)
		return
	}

	status := http.StatusOK
	switch stored.Outcome {
	case domain.DueUnsupported, domain.DueInvalidFacts, domain.DueExtensionNotAllowed:
		status = http.StatusUnprocessableEntity
	case domain.DueAmbiguous, domain.DueNoCalendar:
		status = http.StatusConflict
	}
	body := map[string]any{"decision_id": stored.DecisionID, "replayed": replayed, "calculation": json.RawMessage(stored.Response)}
	if asOf != nil {
		var dd resolver.DueDecision
		if json.Unmarshal(stored.Response, &dd) == nil && dd.Due != nil && dd.Due.DueAt != nil {
			body["as_of"] = asOf.UTC()
			body["overdue"] = dd.Due.OverdueAt(*asOf)
		}
	}
	writeJSON(w, status, body)
}
