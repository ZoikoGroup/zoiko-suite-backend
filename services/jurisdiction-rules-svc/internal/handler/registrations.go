package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
)

// ZS-JUR-001 s16: registration identifier formats, licences, schemes and elections,
// and threshold monitors. Each is a rule of a closed parameter family served from
// verified released packs. The caller sends the facts; the answer is a verdict with its
// explanation, recorded as evidence. Nothing here stores an entity's registration, registers
// anyone or contacts an authority: those facts belong to tenant-entity-registry-svc and the
// Tax domain. A verdict is 200 (valid or invalid, eligible or not, breached or not); a refusal
// to answer (no parameters, unusable facts, unsupported jurisdiction) is 422.

func registerRegistrationRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/registration-identifiers:validate", h.ValidateRegistrationIdentifier)
	r.Post("/v1/licences:evaluate", h.EvaluateLicence)
	r.Post("/v1/scheme-elections:evaluate", h.EvaluateSchemeElection)
	r.Post("/v1/threshold-monitors:evaluate", h.EvaluateThresholdMonitor)
	r.Post("/v1/registration-obligations:resolve", h.ResolveRegistrationObligations)
}

// regLookup names the rule to resolve for one of the s16 endpoints.
type regLookup struct {
	jurisdiction, ruleDomain, code, family, packRef, packVersion string
	at                                                           time.Time
}

// resolveRegulatedRule resolves the rule and decodes its parameters. ok is false when a response has
// already been written (a resolution failure). params is nil when the rule did not resolve or does not
// carry parameters of the wanted family; the caller then answers with the returned outcome.
func (h *Handler) resolveRegulatedRule(w http.ResponseWriter, r *http.Request, l regLookup) (d *resolver.Decision, params *domain.RuleParameters, outcome string, ok bool) {
	d, err := h.resolver.Resolve(r.Context(), resolver.Request{Jurisdiction: l.jurisdiction, RuleDomain: l.ruleDomain,
		RuleCode: l.code, At: l.at, PackRef: l.packRef, PackVersion: l.packVersion})
	if err != nil {
		h.writeResolveError(w, err, r.Header.Get("X-Correlation-ID"))
		return nil, nil, "", false
	}
	if d.Outcome != domain.OutcomeResolved || d.Rule == nil {
		return d, nil, d.Outcome, true
	}
	p, perr := domain.ParseRuleParameters(d.Rule.Parameters)
	if len(d.Rule.Parameters) == 0 || perr != nil || p.Family != l.family {
		return d, nil, domain.CalcNoParameters, true
	}
	return d, &p, d.Outcome, true
}

func parseDay(w http.ResponseWriter, field, v string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_"+field, field+" must be a date, YYYY-MM-DD")
		return time.Time{}, false
	}
	return t, true
}

type RegistrationIdentifierRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	// RegistrationCode names the registration type (the rule code in the REGISTRATION_TYPE domain).
	RegistrationCode string `json:"registration_code"`
	Identifier       string `json:"identifier"`
	// AsOf selects the rule in force on that date (YYYY-MM-DD).
	AsOf        string `json:"as_of"`
	PackRef     string `json:"pack_ref"`
	PackVersion string `json:"pack_version"`
}

// recordedIdentifierRequest is what the evidence ledger keeps of an identifier check: a digest, never the identifier.
type recordedIdentifierRequest struct {
	Jurisdiction     string `json:"jurisdiction"`
	RegistrationCode string `json:"registration_code"`
	IdentifierDigest string `json:"identifier_digest"`
	AsOf             string `json:"as_of"`
	PackRef          string `json:"pack_ref,omitempty"`
	PackVersion      string `json:"pack_version,omitempty"`
}

type RegistrationIdentifierResponse struct {
	Outcome      string                     `json:"outcome"`
	Resolution   *resolver.Decision         `json:"resolution"`
	Registration *domain.RegistrationResult `json:"registration,omitempty"`
}

// ValidateRegistrationIdentifier checks an identifier's format against the registration type in force.
// It does not say whether the number is registered, and it never stores the identifier.
//
//	200 IDENTIFIER_VALID, IDENTIFIER_INVALID or NO_RULE      409 AMBIGUOUS
//	422 UNSUPPORTED_JURISDICTION or NO_PARAMETERS      503 pack_unverified
func (h *Handler) ValidateRegistrationIdentifier(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "registration_identifier", "validate")
	if !ok {
		return
	}
	var req RegistrationIdentifierRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"registration_code", req.RegistrationCode},
		requiredField{"identifier", req.Identifier}, requiredField{"as_of", req.AsOf}); field != "" {
		writeMissingField(w, field)
		return
	}
	if len(req.Identifier) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_identifier", "identifier is at most 256 characters")
		return
	}
	day, ok := parseDay(w, "as_of", req.AsOf)
	if !ok {
		return
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	rawReq, _ := json.Marshal(recordedIdentifierRequest{Jurisdiction: req.Jurisdiction, RegistrationCode: req.RegistrationCode,
		IdentifierDigest: domain.IdentifierDigest(req.Identifier), AsOf: req.AsOf, PackRef: req.PackRef, PackVersion: req.PackVersion})
	d, p, outcome, ok := h.resolveRegulatedRule(w, r, regLookup{jurisdiction: req.Jurisdiction, ruleDomain: domain.RegistrationTypeDomain,
		code: req.RegistrationCode, family: domain.FamilyRegistrationType, packRef: req.PackRef, packVersion: req.PackVersion, at: day})
	if !ok {
		return
	}
	out := RegistrationIdentifierResponse{Outcome: outcome, Resolution: d}
	if p != nil {
		res := domain.CheckIdentifier(*p.Registration, req.Identifier)
		out.Outcome, out.Registration = res.Outcome, &res
	}
	h.finishRecordDecision(w, r, "REGISTRATION", actor, idemKey, rawReq, day, d, out.Outcome, out, "registration")
}

type LicenceRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	LicenceCode  string `json:"licence_code"`
	IssueDate    string `json:"issue_date"`
	// ExpiryDate is the last valid day as printed on the licence. Optional when the type has a fixed term.
	ExpiryDate  *string `json:"expiry_date"`
	AsOf        string  `json:"as_of"`
	PackRef     string  `json:"pack_ref"`
	PackVersion string  `json:"pack_version"`
}

type LicenceResponse struct {
	Outcome    string                `json:"outcome"`
	Resolution *resolver.Decision    `json:"resolution"`
	Licence    *domain.LicenceResult `json:"licence,omitempty"`
}

// EvaluateLicence derives a licence's validity window and status as of a date.
//
//	200 LICENCE_EVALUATED or NO_RULE      409 AMBIGUOUS
//	422 UNSUPPORTED_JURISDICTION, INVALID_FACTS, LICENCE_TERM_EXCEEDED or NO_PARAMETERS      503 pack_unverified
func (h *Handler) EvaluateLicence(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "licence", "evaluate")
	if !ok {
		return
	}
	var req LicenceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"licence_code", req.LicenceCode},
		requiredField{"issue_date", req.IssueDate}, requiredField{"as_of", req.AsOf}); field != "" {
		writeMissingField(w, field)
		return
	}
	issue, ok := parseDay(w, "issue_date", req.IssueDate)
	if !ok {
		return
	}
	asOf, ok := parseDay(w, "as_of", req.AsOf)
	if !ok {
		return
	}
	var expiry *time.Time
	if req.ExpiryDate != nil {
		e, ok := parseDay(w, "expiry_date", *req.ExpiryDate)
		if !ok {
			return
		}
		expiry = &e
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	rawReq, _ := json.Marshal(req)
	d, p, outcome, ok := h.resolveRegulatedRule(w, r, regLookup{jurisdiction: req.Jurisdiction, ruleDomain: domain.LicenceTypeDomain,
		code: req.LicenceCode, family: domain.FamilyLicenceType, packRef: req.PackRef, packVersion: req.PackVersion, at: asOf})
	if !ok {
		return
	}
	out := LicenceResponse{Outcome: outcome, Resolution: d}
	if p != nil {
		res := domain.EvaluateLicence(*p.Licence, issue, expiry, asOf)
		out.Outcome, out.Licence = res.Outcome, &res
	}
	h.finishRecordDecision(w, r, "LICENCE", actor, idemKey, rawReq, asOf, d, out.Outcome, out, "licence")
}

type SchemeElectionRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	SchemeCode   string `json:"scheme_code"`
	// MetricValue is the entity's value of the scheme's eligibility metric, a non-negative decimal string.
	MetricValue string `json:"metric_value"`
	StartDate   string `json:"start_date"`
	// CancelDate, when given, asks when a cancellation made on that date could take effect.
	CancelDate  *string `json:"cancel_date"`
	AsOf        string  `json:"as_of"`
	PackRef     string  `json:"pack_ref"`
	PackVersion string  `json:"pack_version"`
}

type SchemeElectionResponse struct {
	Outcome    string               `json:"outcome"`
	Resolution *resolver.Decision   `json:"resolution"`
	Scheme     *domain.SchemeResult `json:"scheme,omitempty"`
}

// EvaluateSchemeElection checks whether an entity is eligible to elect a scheme and, optionally, when a
// cancellation could take effect given the lock-in and notice rules. It records no election.
//
//	200 SCHEME_ELIGIBLE, SCHEME_NOT_ELIGIBLE or NO_RULE      409 AMBIGUOUS
//	422 UNSUPPORTED_JURISDICTION, INVALID_FACTS or NO_PARAMETERS      503 pack_unverified
func (h *Handler) EvaluateSchemeElection(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "scheme_election", "evaluate")
	if !ok {
		return
	}
	var req SchemeElectionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"scheme_code", req.SchemeCode},
		requiredField{"metric_value", req.MetricValue}, requiredField{"start_date", req.StartDate}, requiredField{"as_of", req.AsOf}); field != "" {
		writeMissingField(w, field)
		return
	}
	start, ok := parseDay(w, "start_date", req.StartDate)
	if !ok {
		return
	}
	asOf, ok := parseDay(w, "as_of", req.AsOf)
	if !ok {
		return
	}
	var cancel *time.Time
	if req.CancelDate != nil {
		c, ok := parseDay(w, "cancel_date", *req.CancelDate)
		if !ok {
			return
		}
		cancel = &c
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	rawReq, _ := json.Marshal(req)
	d, p, outcome, ok := h.resolveRegulatedRule(w, r, regLookup{jurisdiction: req.Jurisdiction, ruleDomain: domain.SchemeDomain,
		code: req.SchemeCode, family: domain.FamilyScheme, packRef: req.PackRef, packVersion: req.PackVersion, at: asOf})
	if !ok {
		return
	}
	out := SchemeElectionResponse{Outcome: outcome, Resolution: d}
	if p != nil {
		res := domain.EvaluateScheme(*p.Scheme, req.MetricValue, start, cancel)
		out.Outcome, out.Scheme = res.Outcome, &res
	}
	h.finishRecordDecision(w, r, "SCHEME", actor, idemKey, rawReq, asOf, d, out.Outcome, out, "scheme")
}

type ThresholdMonitorRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	MonitorCode  string `json:"monitor_code"`
	// AsOf is the evaluation date. Observations dated after it are ignored.
	AsOf         string               `json:"as_of"`
	Observations []domain.Observation `json:"observations"`
	PackRef      string               `json:"pack_ref"`
	PackVersion  string               `json:"pack_version"`
}

type ThresholdMonitorResponse struct {
	Outcome    string                  `json:"outcome"`
	Resolution *resolver.Decision      `json:"resolution"`
	Threshold  *domain.ThresholdResult `json:"threshold,omitempty"`
}

// EvaluateThresholdMonitor sums dated observations over the monitor's period and reports whether, and from
// which date, the threshold was met and when that takes effect. A REGISTRATION_REQUIRED breach returns the
// registration_effective_date the obligation engine takes as its registration_date fact.
//
//	200 THRESHOLD_EVALUATED or NO_RULE      409 AMBIGUOUS
//	422 UNSUPPORTED_JURISDICTION, INVALID_FACTS or NO_PARAMETERS      503 pack_unverified
func (h *Handler) EvaluateThresholdMonitor(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "threshold_monitor", "evaluate")
	if !ok {
		return
	}
	var req ThresholdMonitorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"monitor_code", req.MonitorCode},
		requiredField{"as_of", req.AsOf}); field != "" {
		writeMissingField(w, field)
		return
	}
	asOf, ok := parseDay(w, "as_of", req.AsOf)
	if !ok {
		return
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	rawReq, _ := json.Marshal(req)
	d, p, outcome, ok := h.resolveRegulatedRule(w, r, regLookup{jurisdiction: req.Jurisdiction, ruleDomain: domain.ThresholdDomain,
		code: req.MonitorCode, family: domain.FamilyThresholdMonitor, packRef: req.PackRef, packVersion: req.PackVersion, at: asOf})
	if !ok {
		return
	}
	out := ThresholdMonitorResponse{Outcome: outcome, Resolution: d}
	if p != nil {
		res := domain.EvaluateThreshold(*p.Monitor, req.Observations, asOf)
		out.Outcome, out.Threshold = res.Outcome, &res
	}
	h.finishRecordDecision(w, r, "THRESHOLD", actor, idemKey, rawReq, asOf, d, out.Outcome, out, "threshold")
}
