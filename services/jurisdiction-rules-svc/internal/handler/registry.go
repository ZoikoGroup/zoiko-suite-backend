package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 0: HTTP surface of the regime, source, interpretation and
// pack registries and of rule provenance. Mounted only when a RegistryStore
// is supplied via WithRegistry, so the original constructor and every
// existing route are unchanged.

// RegistryStore is the persistence the registry handlers depend on.
type RegistryStore interface {
	CreateRegime(ctx context.Context, p domain.CreateRegimeParams) (*domain.Regime, bool, error)
	GetRegime(ctx context.Context, idOrCode string) (*domain.Regime, error)
	ListRegimes(ctx context.Context) ([]*domain.Regime, error)

	CreateSource(ctx context.Context, p domain.CreateSourceParams) (*domain.RegulatorySource, bool, error)
	GetSource(ctx context.Context, id string) (*domain.RegulatorySource, error)
	ListSources(ctx context.Context, jurisdictionID, authority string, limit, offset int) ([]*domain.RegulatorySource, error)
	ReviewSource(ctx context.Context, id, reviewer string) (*domain.RegulatorySource, bool, error)
	SupersedeSource(ctx context.Context, id, replacementID string) (*domain.RegulatorySource, bool, error)

	CreateInterpretation(ctx context.Context, p domain.CreateInterpretationParams) (*domain.InterpretationRecord, error)
	GetInterpretation(ctx context.Context, id string) (*domain.InterpretationRecord, error)
	ApproveInterpretation(ctx context.Context, id, approver string) (*domain.InterpretationRecord, bool, error)

	CreatePack(ctx context.Context, p domain.CreatePackParams) (*domain.Pack, bool, error)
	GetPack(ctx context.Context, ref string) (*domain.Pack, error)
	ListPacks(ctx context.Context, limit, offset int) ([]*domain.Pack, error)
	CreatePackVersion(ctx context.Context, p domain.CreatePackVersionParams) (*domain.PackVersion, bool, error)
	GetPackVersion(ctx context.Context, ref, version string) (*domain.PackVersion, error)
	ListPackVersions(ctx context.Context, ref string) ([]*domain.PackVersion, error)
	SubmitPackVersionForReview(ctx context.Context, ref, version, actor string) (*domain.PackVersion, bool, error)

	SetRuleProvenance(ctx context.Context, p domain.SetRuleProvenanceParams) (*domain.RuleProvenance, error)
	GetRuleProvenance(ctx context.Context, ruleID string) (*domain.RuleProvenance, error)

	// ZS-JUR-001 Wave 1.
	RegisterSigningKey(ctx context.Context, keyRef, algorithm string, pub []byte, actor string) (*domain.TrustedKey, bool, error)
	GetSigningKey(ctx context.Context, keyRef string) (*domain.TrustedKey, error)
	ListSigningKeys(ctx context.Context) ([]*domain.TrustedKey, error)
	SetSigningKeyStatus(ctx context.Context, keyRef, status, reason, actor string) (*domain.TrustedKey, bool, error)
	CompilePackVersion(ctx context.Context, ref, version, actor string) (*store.PackArtifact, bool, error)
	GetPackArtifact(ctx context.Context, ref, version string) (*store.PackArtifact, error)
	SignPackVersion(ctx context.Context, ref, version string, signer domain.Signer, actor string) (*store.PackArtifact, bool, error)
	VerifyPackArtifact(ctx context.Context, ref, version string) (*store.PackArtifact, domain.VerificationResult, error)

	// ZS-JUR-001 Wave 3.
	SubmitTestBundle(ctx context.Context, ref, version string, canonical []byte, digest, actor string) (*store.TestBundleRecord, bool, error)
	GetLatestTestBundle(ctx context.Context, ref, version string) (*store.TestBundleRecord, error)
	RunTests(ctx context.Context, ref, version, actor string) (*store.TestRunRecord, error)
	ListTestRuns(ctx context.Context, ref, version string, limit int) ([]*store.TestRunRecord, error)
	AddReview(ctx context.Context, ref, version, reviewer, role, decision, findings string) (*store.ReviewRecord, error)
	ListReviews(ctx context.Context, ref, version string) ([]*store.ReviewRecord, error)
	CertifyPackVersion(ctx context.Context, ref, version, certifier string, minReviews int, signer domain.Signer) (*store.CertificationRecord, bool, error)
	GetCertification(ctx context.Context, ref, version string) (*store.CertificationRecord, domain.VerificationResult, error)

	// ZS-JUR-001 Wave 2.
	RecordDecision(ctx context.Context, rec store.DecisionRecord) (*store.StoredDecision, bool, error)
	GetDecision(ctx context.Context, id string) (*store.StoredDecision, error)

	// ZS-JUR-001 Wave 8 (rollout).
	CreateRollout(ctx context.Context, p store.CreateRolloutParams) (*store.RolloutRecord, error)
	SeedPortfolio(ctx context.Context, actor string) (int, int, error)
	ListRollouts(ctx context.Context, status string, limit, offset int) ([]*store.RolloutRecord, error)
	GetRollout(ctx context.Context, id string) (*store.RolloutDetail, error)
	SetRolloutOwner(ctx context.Context, id, owner string, supportOwner *string) (*store.RolloutRecord, error)
	LinkRolloutPacks(ctx context.Context, id string, packRefs []string, actor string) error
	AddAttestation(ctx context.Context, id, checklist, item string, met bool, evidence, actor string) error
	AddExpertApproval(ctx context.Context, id, expert, qualification, scope, decision, notes string) error
	TransitionRollout(ctx context.Context, id, to, actor, reason string) (*store.RolloutRecord, error)
	JurisdictionSupport(ctx context.Context, code string) (*store.SupportView, error)

	// ZS-JUR-001 Wave 5 (submissions).
	CreateSubmission(ctx context.Context, p store.CreateSubmissionParams) (*store.SubmissionRecord, bool, error)
	GetSubmission(ctx context.Context, id string) (*store.SubmissionRecord, []*store.SubmissionEventRecord, error)
	RecordSubmissionEvent(ctx context.Context, id string, p store.SubmissionEventParams) (*store.SubmissionEventResult, error)

	// ZS-JUR-001 Wave 4 (tax half).
	SetRuleParameters(ctx context.Context, ruleID string, params json.RawMessage, actor string) (json.RawMessage, error)
	GetRuleParameters(ctx context.Context, ruleID string) (json.RawMessage, error)

	// ZS-JUR-001 Wave 4 (calendar half).
	CreateCalendar(ctx context.Context, code, jurisdictionID, authority string, description *string, actor string) (*store.Calendar, bool, error)
	GetCalendar(ctx context.Context, code string) (*store.Calendar, error)
	ListCalendars(ctx context.Context) ([]*store.Calendar, error)
	CreateCalendarVersion(ctx context.Context, code, effectiveFrom, tz string, weekend []int, cutoff *string, holidays []domain.Holiday, actor string) (*store.CalendarVersionRecord, error)
	GetCalendarVersion(ctx context.Context, code string, version int) (*store.CalendarVersionRecord, error)
	ListCalendarVersions(ctx context.Context, code string) ([]*store.CalendarVersionRecord, error)
	SetCalendarVersionSources(ctx context.Context, code string, version int, sourceIDs []string) (*store.CalendarVersionRecord, error)
	PublishCalendarVersion(ctx context.Context, code string, version int, actor string) (*store.CalendarVersionRecord, bool, error)
	CreateObligationRule(ctx context.Context, p store.CreateObligationParams) (*store.ObligationRuleRecord, error)
	GetObligationRule(ctx context.Context, id string) (*store.ObligationRuleRecord, error)
	ListObligationRules(ctx context.Context, jurisdictionID, code string, limit, offset int) ([]*store.ObligationRuleRecord, error)
	SetObligationProvenance(ctx context.Context, id string, regimeID, interpretationID *string, sourceIDs []string) (*store.ObligationRuleRecord, error)
	PublishObligationRule(ctx context.Context, id, actor string) (*store.ObligationRuleRecord, bool, error)

	// ZS-JUR-001 Wave 7.
	CreateRegion(ctx context.Context, code string, description *string, actor string) (*store.Region, bool, error)
	ListRegions(ctx context.Context) ([]*store.Region, error)
	CreateRing(ctx context.Context, code string, ordinal, soak int, description *string, actor string) (*store.Ring, bool, error)
	ListRings(ctx context.Context) ([]*store.Ring, error)
	PublishPackVersion(ctx context.Context, ref, version, actor, evidenceRef string) (*store.ReleaseResult, error)
	WithdrawPackVersion(ctx context.Context, ref, version, reason, evidenceRef, actor string) (*store.ReleaseResult, error)
	BlockPackVersion(ctx context.Context, ref, version, reason, actor string) (*store.ReleaseResult, error)
	UnblockPackVersion(ctx context.Context, ref, version, reason, actor string) (*store.ReleaseResult, error)
	DeployPackVersion(ctx context.Context, ref, version, ring, region, actor, exceptionReason string) (*store.Deployment, bool, error)
	RollbackDeployment(ctx context.Context, ref, version, ring, region, restoreVersion, reason, actor string) (*store.RollbackResult, error)
	ListDeployments(ctx context.Context, ref, version string) ([]*store.Deployment, error)
	DeclareHotfix(ctx context.Context, ref, version, severity, scope, incident, commander, rollbackTarget string, retroSLA time.Duration, actor string) (*store.Hotfix, bool, error)
	GetHotfix(ctx context.Context, ref, version string) (*store.Hotfix, error)
	CompleteRetrospective(ctx context.Context, ref, version, note, actor string) (*store.Hotfix, bool, error)
	CreateNotice(ctx context.Context, p store.CreateNoticeParams) (*store.SourceChangeNotice, error)
	GetNotice(ctx context.Context, id string) (*store.SourceChangeNotice, error)
	ListNotices(ctx context.Context, status string, limit, offset int) ([]*store.SourceChangeNotice, error)
	StartNoticeReview(ctx context.Context, id, reviewer string) (*store.SourceChangeNotice, bool, error)
	CloseNotice(ctx context.Context, id, closer, outcome, note string, linkedInterpretation *string) (*store.SourceChangeNotice, bool, error)
	Metrics(ctx context.Context, windowHours, certAgeWarnDays, upcomingDays int) (*store.OpsMetrics, error)
}

// WithRegistry enables the ZS-JUR-001 registry routes. pub may be nil, in
// which case registry events are not emitted.
func (h *Handler) WithRegistry(rs RegistryStore, pub events.RegistryPublisher) *Handler {
	h.registry = rs
	h.registryPub = pub
	return h
}

func registerRegistryRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/regimes", h.ListRegimes)
	r.Get("/v1/regimes/{regime}", h.GetRegime)
	r.Get("/v1/sources", h.ListSources)
	r.Get("/v1/sources/{source_id}", h.GetSource)
	r.Get("/v1/interpretations/{interpretation_id}", h.GetInterpretation)
	r.Get("/v1/packs", h.ListPacks)
	r.Get("/v1/packs/{pack_ref}", h.GetPack)
	r.Get("/v1/packs/{pack_ref}/versions", h.ListPackVersions)
	r.Get("/v1/packs/{pack_ref}/versions/{version}", h.GetPackVersion)
	r.Get("/v1/rules/{jurisdiction_rule_id}/provenance", h.GetRuleProvenance)

	r.Post("/v1/admin/regimes", h.CreateRegime)
	r.Post("/v1/admin/sources", h.CreateSource)
	r.Post("/v1/admin/sources/{source_id}/review", h.ReviewSource)
	r.Post("/v1/admin/sources/{source_id}/supersede", h.SupersedeSource)
	r.Post("/v1/admin/interpretations", h.CreateInterpretation)
	r.Post("/v1/admin/interpretations/{interpretation_id}/approve", h.ApproveInterpretation)
	r.Post("/v1/admin/packs", h.CreatePack)
	r.Post("/v1/admin/packs/{pack_ref}/versions", h.CreatePackVersion)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/submit-review", h.SubmitPackVersionForReview)
	r.Put("/v1/admin/rules/{jurisdiction_rule_id}/provenance", h.SetRuleProvenance)

	registerCompileRoutes(r, h)
	registerCertRoutes(r, h)
	registerOpsRoutes(r, h)
	registerCalendarRoutes(r, h)
	registerTaxRoutes(r, h)
	registerRolloutRoutes(r, h)
	if h.resolver != nil {
		registerResolverRoutes(r, h)
	}
}

// admin authenticates and authorizes a privileged registry command.
// Authoring, review, approval and publication are distinct actions so the
// platform can grant them to different roles (ZS-JUR-001 s28).
func (h *Handler) admin(w http.ResponseWriter, r *http.Request, resource, action string) (string, bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return "", false
	}
	if err := h.checkAuthz(r, principalID, resource, action); err != nil {
		h.writeAuthzError(w, err)
		return "", false
	}
	return principalID, true
}

func (h *Handler) emitRegistry(r *http.Request, eventType, aggregateID, actor string, payload map[string]any) {
	if h.registryPub == nil {
		return
	}
	corr := r.Header.Get("X-Correlation-ID")
	h.publish(eventType, corr, func() error {
		return h.registryPub.PublishRegistryEvent(r.Context(), eventType, aggregateID, actor, corr, payload)
	})
}

// writeRegistryError maps registry errors, then defers to the shared mapper.
func (h *Handler) writeRegistryError(w http.ResponseWriter, err error, corr string) {
	switch {
	case errors.Is(err, domain.ErrRegimeNotFound):
		writeError(w, http.StatusNotFound, "regime_not_found", "no such regulatory regime")
	case errors.Is(err, domain.ErrSourceNotFound):
		writeError(w, http.StatusNotFound, "source_not_found", "no such regulatory source")
	case errors.Is(err, domain.ErrInterpretationNotFound):
		writeError(w, http.StatusNotFound, "interpretation_not_found", "no such interpretation record")
	case errors.Is(err, domain.ErrPackNotFound):
		writeError(w, http.StatusNotFound, "pack_not_found", "no such jurisdiction pack")
	case errors.Is(err, domain.ErrPackVersionNotFound):
		writeError(w, http.StatusNotFound, "pack_version_not_found", "no such pack version")
	case errors.Is(err, domain.ErrNotIndependent):
		writeError(w, http.StatusForbidden, "segregation_of_duties", err.Error())
	case errors.Is(err, domain.ErrAlreadyDecided):
		writeError(w, http.StatusConflict, "already_decided", err.Error())
	case errors.Is(err, domain.ErrNotDraft):
		writeError(w, http.StatusConflict, "not_draft", err.Error())
	case errors.Is(err, domain.ErrVersionNotNewer):
		writeError(w, http.StatusConflict, "version_not_newer", err.Error())
	case errors.Is(err, domain.ErrUnapprovedInterpretation):
		writeError(w, http.StatusConflict, "interpretation_not_approved", err.Error())
	case errors.Is(err, domain.ErrSourceNotReady):
		writeError(w, http.StatusConflict, "source_not_ready", err.Error())
	case errors.Is(err, domain.ErrInvalidReference):
		writeError(w, http.StatusBadRequest, "invalid_reference", err.Error())
	case errors.Is(err, domain.ErrManifestInvalid):
		writeError(w, http.StatusBadRequest, "invalid_manifest", err.Error())
	default:
		h.writeStoreError(w, err, corr)
	}
}

var keyRefRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]*$`)

func parseDate(s string) (time.Time, error) { return time.Parse("2006-01-02", s) }

// ── regimes ─────────────────────────────────────────────────────────────────

type CreateRegimeRequest struct {
	RegimeCode  string  `json:"regime_code"`
	RegimeName  string  `json:"regime_name"`
	Description *string `json:"description"`
}

func (h *Handler) CreateRegime(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_regime", "create")
	if !ok {
		return
	}
	var req CreateRegimeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"regime_code", req.RegimeCode}, requiredField{"regime_name", req.RegimeName}); field != "" {
		writeMissingField(w, field)
		return
	}
	x, created, err := h.registry.CreateRegime(r.Context(), domain.CreateRegimeParams{
		RegimeCode: strings.ToUpper(strings.TrimSpace(req.RegimeCode)), RegimeName: req.RegimeName,
		Description: req.Description, CreatedBy: actor})
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventRegimeCreated, x.RegimeID, actor,
			map[string]any{"regime_id": x.RegimeID, "regime_code": x.RegimeCode, "regime_name": x.RegimeName})
		writeJSON(w, http.StatusCreated, x)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *Handler) GetRegime(w http.ResponseWriter, r *http.Request) {
	x, err := h.registry.GetRegime(r.Context(), chi.URLParam(r, "regime"))
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *Handler) ListRegimes(w http.ResponseWriter, r *http.Request) {
	xs, err := h.registry.ListRegimes(r.Context())
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"regimes": xs})
}

// ── source register ─────────────────────────────────────────────────────────

type CreateSourceRequest struct {
	JurisdictionID      string  `json:"jurisdiction_id"`
	Authority           string  `json:"authority"`
	SourceType          string  `json:"source_type"`
	AuthorityLevel      string  `json:"authority_level"`
	Title               string  `json:"title"`
	OfficialIdentifier  *string `json:"official_identifier"`
	PublishedOn         *string `json:"published_on"`
	EffectiveOn         *string `json:"effective_on"`
	Location            string  `json:"location"`
	SnapshotHash        string  `json:"snapshot_hash"`
	SnapshotRef         *string `json:"snapshot_ref"`
	Language            string  `json:"language"`
	TranslationRef      *string `json:"translation_ref"`
	InterpretationNotes *string `json:"interpretation_notes"`
}

func (h *Handler) CreateSource(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_source", "create")
	if !ok {
		return
	}
	var req CreateSourceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p := domain.CreateSourceParams{
		JurisdictionID: req.JurisdictionID, Authority: req.Authority, SourceType: req.SourceType,
		AuthorityLevel: req.AuthorityLevel, Title: req.Title, OfficialIdentifier: req.OfficialIdentifier,
		PublishedOn: req.PublishedOn, EffectiveOn: req.EffectiveOn, Location: req.Location,
		SnapshotHash: req.SnapshotHash, SnapshotRef: req.SnapshotRef, Language: req.Language,
		TranslationRef: req.TranslationRef, InterpretationNotes: req.InterpretationNotes, CreatedBy: actor,
	}
	if err := domain.ValidateSource(p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
		return
	}
	x, created, err := h.registry.CreateSource(r.Context(), p)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventSourceCaptured, x.SourceID, actor, map[string]any{
			"source_id": x.SourceID, "jurisdiction_id": x.JurisdictionID, "authority": x.Authority,
			"source_type": x.SourceType, "authority_level": x.AuthorityLevel, "snapshot_hash": x.SnapshotHash})
		writeJSON(w, http.StatusCreated, x)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *Handler) GetSource(w http.ResponseWriter, r *http.Request) {
	x, err := h.registry.GetSource(r.Context(), chi.URLParam(r, "source_id"))
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *Handler) ListSources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset, ok := h.parsePaging(w, q)
	if !ok {
		return
	}
	xs, err := h.registry.ListSources(r.Context(), q.Get("jurisdiction_id"), q.Get("authority"), limit, offset)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": xs})
}

// ReviewSource records the independent source review (s8 "reviewed_by").
func (h *Handler) ReviewSource(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_source", "review")
	if !ok {
		return
	}
	x, changed, err := h.registry.ReviewSource(r.Context(), chi.URLParam(r, "source_id"), actor)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventSourceReviewed, x.SourceID, actor,
			map[string]any{"source_id": x.SourceID, "jurisdiction_id": x.JurisdictionID})
	}
	writeJSON(w, http.StatusOK, x)
}

type SupersedeSourceRequest struct {
	ReplacementSourceID string `json:"replacement_source_id"`
}

func (h *Handler) SupersedeSource(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_source", "supersede")
	if !ok {
		return
	}
	var req SupersedeSourceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ReplacementSourceID) == "" {
		writeMissingField(w, "replacement_source_id")
		return
	}
	x, changed, err := h.registry.SupersedeSource(r.Context(), chi.URLParam(r, "source_id"), req.ReplacementSourceID)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventSourceSuperseded, x.SourceID, actor, map[string]any{
			"source_id": x.SourceID, "jurisdiction_id": x.JurisdictionID, "superseded_by_source_id": x.SupersededBySourceID})
	}
	writeJSON(w, http.StatusOK, x)
}

// ── interpretation records ──────────────────────────────────────────────────

type CreateInterpretationRequest struct {
	JurisdictionID string   `json:"jurisdiction_id"`
	RegimeID       *string  `json:"regime_id"`
	Subject        string   `json:"subject"`
	Decision       string   `json:"decision"`
	Rationale      string   `json:"rationale"`
	SourceIDs      []string `json:"source_ids"`
}

func (h *Handler) CreateInterpretation(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "interpretation_record", "create")
	if !ok {
		return
	}
	var req CreateInterpretationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction_id", req.JurisdictionID}, requiredField{"subject", req.Subject},
		requiredField{"decision", req.Decision}, requiredField{"rationale", req.Rationale}); field != "" {
		writeMissingField(w, field)
		return
	}
	if len(req.SourceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "missing_sources", "an interpretation must cite at least one authoritative source")
		return
	}
	x, err := h.registry.CreateInterpretation(r.Context(), domain.CreateInterpretationParams{
		JurisdictionID: req.JurisdictionID, RegimeID: req.RegimeID, Subject: req.Subject, Decision: req.Decision,
		Rationale: req.Rationale, SourceIDs: req.SourceIDs, CreatedBy: actor})
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	h.emitRegistry(r, events.EventInterpretationRecorded, x.InterpretationID, actor,
		map[string]any{"interpretation_id": x.InterpretationID, "jurisdiction_id": x.JurisdictionID, "status": x.Status})
	writeJSON(w, http.StatusCreated, x)
}

func (h *Handler) GetInterpretation(w http.ResponseWriter, r *http.Request) {
	x, err := h.registry.GetInterpretation(r.Context(), chi.URLParam(r, "interpretation_id"))
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, x)
}

// ApproveInterpretation is the qualified-human gate (s3, JUR-NEG-18): the
// approver must not be the author and the cited sources must be reviewed.
func (h *Handler) ApproveInterpretation(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "interpretation_record", "approve")
	if !ok {
		return
	}
	x, changed, err := h.registry.ApproveInterpretation(r.Context(), chi.URLParam(r, "interpretation_id"), actor)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventInterpretationApproved, x.InterpretationID, actor,
			map[string]any{"interpretation_id": x.InterpretationID, "jurisdiction_id": x.JurisdictionID})
	}
	writeJSON(w, http.StatusOK, x)
}

// ── packs ───────────────────────────────────────────────────────────────────

type CreatePackRequest struct {
	PackRef      string  `json:"pack_ref"`
	PackName     string  `json:"pack_name"`
	Owner        string  `json:"owner"`
	SupportOwner *string `json:"support_owner"`
}

func (h *Handler) CreatePack(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack", "create")
	if !ok {
		return
	}
	var req CreatePackRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"pack_ref", req.PackRef}, requiredField{"pack_name", req.PackName},
		requiredField{"owner", req.Owner}); field != "" {
		writeMissingField(w, field)
		return
	}
	if !domain.ValidPackRef(req.PackRef) {
		writeError(w, http.StatusBadRequest, "invalid_pack_ref", "pack_ref must look like jur.gb.tax.core")
		return
	}
	x, created, err := h.registry.CreatePack(r.Context(), domain.CreatePackParams{
		PackRef: req.PackRef, PackName: req.PackName, Owner: req.Owner, SupportOwner: req.SupportOwner, CreatedBy: actor})
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventPackCreated, x.PackID, actor, map[string]any{"pack_id": x.PackID, "pack_ref": x.PackRef})
		writeJSON(w, http.StatusCreated, x)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *Handler) GetPack(w http.ResponseWriter, r *http.Request) {
	x, err := h.registry.GetPack(r.Context(), chi.URLParam(r, "pack_ref"))
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *Handler) ListPacks(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.parsePaging(w, r.URL.Query())
	if !ok {
		return
	}
	xs, err := h.registry.ListPacks(r.Context(), limit, offset)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"packs": xs})
}

// CreatePackVersion drafts a version from a manifest sent as the request body.
// Status, digests and signature are server-owned: a manifest that asserts
// them is rejected as an unknown field.
func (h *Handler) CreatePackVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "create")
	if !ok {
		return
	}
	ref := chi.URLParam(r, "pack_ref")
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "manifest too large")
		return
	}
	m, err := domain.ParseManifest(raw)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if m.PackID != ref {
		writeError(w, http.StatusBadRequest, "pack_mismatch", "manifest pack_id must equal the pack in the URL")
		return
	}
	canonical, err := domain.CanonicalJSON(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_manifest", err.Error())
		return
	}
	digest, err := domain.DigestOf(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_manifest", err.Error())
		return
	}
	v, created, err := h.registry.CreatePackVersion(r.Context(), domain.CreatePackVersionParams{
		PackRef: ref, Manifest: m, ManifestJSON: canonical, ManifestDigest: digest, CreatedBy: actor})
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventPackVersionDrafted, v.PackVersionID, actor, map[string]any{
			"pack_version_id": v.PackVersionID, "pack_ref": v.PackRef, "version": v.Version, "manifest_digest": v.ManifestDigest})
		writeJSON(w, http.StatusCreated, v)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) GetPackVersion(w http.ResponseWriter, r *http.Request) {
	v, err := h.registry.GetPackVersion(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"))
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) ListPackVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := h.registry.ListPackVersions(r.Context(), chi.URLParam(r, "pack_ref"))
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

func (h *Handler) SubmitPackVersionForReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "submit")
	if !ok {
		return
	}
	v, changed, err := h.registry.SubmitPackVersionForReview(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"), actor)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventPackVersionSubmitted, v.PackVersionID, actor,
			map[string]any{"pack_version_id": v.PackVersionID, "pack_ref": v.PackRef, "version": v.Version})
	}
	writeJSON(w, http.StatusOK, v)
}

// ── rule provenance ─────────────────────────────────────────────────────────

type SetRuleProvenanceRequest struct {
	RegimeID         *string  `json:"regime_id"`
	InterpretationID *string  `json:"interpretation_id"`
	SupersedesRuleID *string  `json:"supersedes_rule_id"`
	Precedence       *int     `json:"precedence"`
	PublishedOn      *string  `json:"published_on"`
	SourceIDs        []string `json:"source_ids"`
}

// SetRuleProvenance replaces the provenance of a DRAFT rule. It is a full
// replacement (PUT): omitted fields are cleared, so the stored provenance is
// always exactly what the caller last stated.
func (h *Handler) SetRuleProvenance(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_rule", "set_provenance")
	if !ok {
		return
	}
	var req SetRuleProvenanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.PublishedOn != nil {
		if _, err := parseDate(*req.PublishedOn); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_published_on", "published_on must be a YYYY-MM-DD date")
			return
		}
	}
	id := chi.URLParam(r, "jurisdiction_rule_id")
	p, err := h.registry.SetRuleProvenance(r.Context(), domain.SetRuleProvenanceParams{
		JurisdictionRuleID: id, RegimeID: req.RegimeID, InterpretationID: req.InterpretationID,
		SupersedesRuleID: req.SupersedesRuleID, Precedence: req.Precedence, PublishedOn: req.PublishedOn,
		SourceIDs: req.SourceIDs, ActorID: actor})
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	h.emitRegistry(r, events.EventRuleProvenanceSet, id, actor,
		map[string]any{"jurisdiction_rule_id": id, "source_count": len(p.SourceIDs)})
	h.log.Info("SetRuleProvenance", zap.String("jurisdiction_rule_id", id), zap.String("principal_id", actor))
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) GetRuleProvenance(w http.ResponseWriter, r *http.Request) {
	p, err := h.registry.GetRuleProvenance(r.Context(), chi.URLParam(r, "jurisdiction_rule_id"))
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, p)
}
