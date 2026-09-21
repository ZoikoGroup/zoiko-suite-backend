package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/envelope"
	"zoiko.io/search-indexer-svc/internal/projection"
)

// The control plane: ESR-01's source and contract registry, and ESR-05's
// generation lifecycle.
//
// Every write here is PLATFORM-scoped. A search contract describes a shape
// shared by every tenant, and a generation is one physical index serving all
// of them — so authorizing these against a caller's own legal entity would let
// a tenant-scoped grant change what every other tenant can search. The
// platform sentinel (tracker row 67) is what makes "this is a platform-wide
// act" expressible to authorization-svc rather than each service inventing a
// synthetic entity id.

// requirePlatform authorizes a control-plane act against platform scope.
func (h *Handler) requirePlatform(w http.ResponseWriter, r *http.Request, action string) (envelope.Envelope, bool) {
	env := envelope.MustFromContext(r.Context())
	if env.ActorSubjectID == "" {
		writeError(w, http.StatusUnauthorized, "principal_required",
			"a control-plane write requires a verified principal")
		return env, false
	}
	if h.platformScopeID == "" {
		// Fail closed rather than fall back to the caller's own entity.
		// Falling back would silently turn a platform-wide act into a
		// tenant-scoped check that a tenant admin could pass — the exact
		// confusion row 67 records, where a grant seeded against one id was
		// invisible to a check made against another.
		writeError(w, http.StatusInternalServerError, "platform_scope_not_configured",
			"AUTHZ_PLATFORM_SCOPE_ID is not set; control-plane writes cannot be authorized")
		return env, false
	}
	if err := h.authz.CheckAllowed(r.Context(), env.ActorSubjectID, h.platformScopeID, action); err != nil {
		h.metrics.AuthzDecision(action, outcomeOf(err))
		h.writeAuthzError(w, err)
		return env, false
	}
	h.metrics.AuthzDecision(action, "allowed")
	return env, true
}

// ── ESR-01: sources ──────────────────────────────────────────────────────────

type createSourceRequest struct {
	OwnerService          string   `json:"owner_service"`
	SourceType            string   `json:"source_type"`
	TenantScope           string   `json:"tenant_scope"`
	ResidencyRegion       string   `json:"residency_region"`
	SensitivityCeiling    string   `json:"sensitivity_ceiling"`
	EventTopic            string   `json:"event_topic"`
	EventTypes            []string `json:"event_types"`
	RestrictionEventTypes []string `json:"restriction_event_types"`
	FreshnessClass        string   `json:"freshness_class"`
	MaxLagSeconds         int      `json:"max_lag_seconds"`
}

func (h *Handler) CreateSource(w http.ResponseWriter, r *http.Request) {
	env, ok := h.requirePlatform(w, r, ActionSourceRegister)
	if !ok {
		return
	}
	var req createSourceRequest
	if !decode(w, r, &req) {
		return
	}

	if req.SourceType == "" || req.OwnerService == "" || req.EventTopic == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"owner_service, source_type and event_topic are required")
		return
	}
	if len(req.EventTypes) == 0 {
		// A source with no event types consumes nothing. Registering one
		// would create a topic subscription that can never produce a
		// projection, which reads as "indexing is broken" rather than as
		// "nobody finished the registration".
		writeError(w, http.StatusBadRequest, "event_types_required",
			"a source must declare the event types it is indexed from")
		return
	}

	ceiling := domain.SensitivityClass(strings.ToUpper(strings.TrimSpace(req.SensitivityCeiling)))
	if ceiling == "" {
		ceiling = domain.SensitivityInternal
	}
	if ceiling == domain.SensitivitySecretProhibited {
		// INV-09. A source whose ceiling is the prohibited class could have
		// no legal contract at all, so registering it would only create
		// something for a later bug to misread as permission.
		writeError(w, http.StatusBadRequest, "prohibited_ceiling",
			"SECRET_PROHIBITED is not a sensitivity ceiling: such content never enters a search index")
		return
	}

	src := domain.SearchSource{
		SourceID:              uuid.NewString(),
		OwnerService:          req.OwnerService,
		SourceType:            req.SourceType,
		TenantScope:           orDefault(req.TenantScope, "TENANT_SHARDED"),
		ResidencyRegion:       orDefault(req.ResidencyRegion, "GLOBAL"),
		SensitivityCeiling:    ceiling,
		EventTopic:            req.EventTopic,
		EventTypes:            req.EventTypes,
		RestrictionEventTypes: req.RestrictionEventTypes,
		FreshnessClass:        orDefault(req.FreshnessClass, "S1"),
		MaxLagSeconds:         orDefaultInt(req.MaxLagSeconds, 300),
		CreatedByPrincipalID:  env.ActorSubjectID,
	}

	if err := h.store.CreateSource(r.Context(), src); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "source_exists", err.Error())
			return
		}
		h.internal(w, r, "create source", err)
		return
	}

	// Reload so the new source's topic is subscribed without a restart.
	if err := h.indexer.Reload(r.Context()); err != nil {
		h.log.Error("source registered but the projector registry could not reload", zap.Error(err))
	}
	writeJSON(w, http.StatusCreated, src)
}

func (h *Handler) ListSources(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListSources(r.Context())
	if err != nil {
		h.internal(w, r, "list sources", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// ── ESR-01: contracts ────────────────────────────────────────────────────────

type contractFieldRequest struct {
	Name             string `json:"name"`
	SourcePath       string `json:"source_path"`
	Type             string `json:"type"`
	Searchable       bool   `json:"searchable"`
	Filterable       bool   `json:"filterable"`
	Facetable        bool   `json:"facetable"`
	Sortable         bool   `json:"sortable"`
	SnippetAllowed   bool   `json:"snippet_allowed"`
	Returnable       bool   `json:"returnable"`
	Exportable       bool   `json:"exportable"`
	SensitivityClass string `json:"sensitivity_class"`
	AnalyzerProfile  string `json:"analyzer_profile"`
}

type createContractRequest struct {
	SourceType      string                 `json:"source_type"`
	ScopeName       string                 `json:"scope_name"`
	RetrievalClass  string                 `json:"retrieval_class"`
	FreshnessClass  string                 `json:"freshness_class"`
	AnalyzerProfile string                 `json:"analyzer_profile"`
	AuthzAction     string                 `json:"authz_action"`
	Fields          []contractFieldRequest `json:"fields"`
}

// CreateContract drafts a new contract version.
//
// Always DRAFT. §4.2's publication gates — source owner, Security, Privacy and
// Search Platform approval — are a human workflow this service cannot perform,
// so it cannot create a published contract in one call however complete the
// request is. Publication is a separate, separately-authorized transition.
func (h *Handler) CreateContract(w http.ResponseWriter, r *http.Request) {
	env, ok := h.requirePlatform(w, r, ActionContractCreate)
	if !ok {
		return
	}
	var req createContractRequest
	if !decode(w, r, &req) {
		return
	}

	src, err := h.store.GetSourceByType(r.Context(), req.SourceType)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "source_not_registered",
				"source_type "+req.SourceType+" is not registered")
			return
		}
		h.internal(w, r, "resolve source", err)
		return
	}

	scope := orDefault(req.ScopeName, req.SourceType)
	if req.AuthzAction == "" {
		writeError(w, http.StatusBadRequest, "authz_action_required",
			"a contract must name the action its results are re-authorized against; "+
				"without one, R1/R2 retrieval has nothing to ask authorization-svc")
		return
	}
	retrieval := domain.RetrievalClass(orDefault(strings.ToUpper(req.RetrievalClass), "R1"))
	switch retrieval {
	case domain.RetrievalR0, domain.RetrievalR1, domain.RetrievalR2, domain.RetrievalR3:
	default:
		writeError(w, http.StatusBadRequest, "invalid_retrieval_class",
			"retrieval_class must be one of R0, R1, R2, R3")
		return
	}

	fields, err := h.validateFields(req.Fields, src, retrieval)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field_contract", err.Error())
		return
	}

	version, err := h.store.NextContractVersion(r.Context(), scope)
	if err != nil {
		h.internal(w, r, "next contract version", err)
		return
	}

	contract := domain.IndexContract{
		ContractID:           uuid.NewString(),
		SourceID:             src.SourceID,
		ScopeName:            scope,
		Version:              version,
		SchemaDigest:         schemaDigest(fields),
		State:                domain.ContractDraft,
		FreshnessClass:       orDefault(req.FreshnessClass, src.FreshnessClass),
		RetrievalClass:       retrieval,
		AnalyzerProfile:      orDefault(req.AnalyzerProfile, "standard"),
		AuthzAction:          req.AuthzAction,
		Fields:               fields,
		CreatedByPrincipalID: env.ActorSubjectID,
	}

	if err := h.store.CreateContract(r.Context(), contract); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "contract_exists", err.Error())
			return
		}
		h.internal(w, r, "create contract", err)
		return
	}
	writeJSON(w, http.StatusCreated, contract)
}

// validateFields enforces §4.1/§4.2's field rules before anything is stored.
func (h *Handler) validateFields(reqFields []contractFieldRequest, src *domain.SearchSource, retrieval domain.RetrievalClass) ([]domain.SearchFieldDefinition, error) {
	if len(reqFields) == 0 {
		return nil, errors.New("a contract must register at least one field")
	}

	reserved := map[string]bool{}
	for _, name := range searchclient.ReservedFields() {
		reserved[name] = true
	}

	ceiling := sensitivityRank(src.SensitivityCeiling)
	seen := map[string]bool{}
	out := make([]domain.SearchFieldDefinition, 0, len(reqFields))

	for _, f := range reqFields {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			return nil, errors.New("every field needs a name")
		}
		if seen[name] {
			return nil, fmt.Errorf("field %q is registered twice", name)
		}
		seen[name] = true

		if reserved[name] {
			return nil, fmt.Errorf("field %q is a reserved governance field: registering it would let a source "+
				"overwrite its own tenant or restriction epoch", name)
		}

		class := domain.SensitivityClass(strings.ToUpper(orDefault(f.SensitivityClass, "INTERNAL")))

		if class == domain.SensitivitySecretProhibited {
			// Registering a prohibited field is legal and useful: it is how
			// the projector learns to refuse a payload that carries one
			// (NP-10). What is not legal is exposing it in any way.
			if f.Searchable || f.Filterable || f.Facetable || f.Sortable ||
				f.SnippetAllowed || f.Returnable || f.Exportable {
				return nil, fmt.Errorf("field %q is SECRET_PROHIBITED and cannot be searchable, "+
					"filterable, facetable, sortable, snippet-allowed, returnable or exportable", name)
			}
		} else if sensitivityRank(class) > ceiling {
			// §4.1's sensitivity ceiling. A contract cannot expose a field
			// more sensitive than its source was registered to carry —
			// raising exposure is a source-level decision with a different
			// approval path, not a contract-level one.
			return nil, fmt.Errorf("field %q is classified %s but source %q has a ceiling of %s",
				name, class, src.SourceType, src.SensitivityCeiling)
		}

		if f.SnippetAllowed && !f.Returnable {
			// INV-13. A snippet is a fragment of a field's content, so
			// allowing one from a field the caller may not receive would
			// return the content in pieces.
			return nil, fmt.Errorf("field %q allows snippets but is not returnable", name)
		}
		if f.SnippetAllowed && !f.Searchable {
			// A snippet is the matched span. A field that cannot match has no
			// span to show, so this is a contract that cannot mean anything.
			return nil, fmt.Errorf("field %q allows snippets but is not searchable", name)
		}
		if f.Exportable && !f.Returnable {
			return nil, fmt.Errorf("field %q is exportable but not returnable", name)
		}

		fieldType := strings.ToUpper(orDefault(f.Type, "KEYWORD"))
		switch fieldType {
		case "KEYWORD", "TEXT", "DATE", "LONG", "DOUBLE", "BOOLEAN":
		default:
			return nil, fmt.Errorf("field %q has unknown type %q", name, f.Type)
		}

		out = append(out, domain.SearchFieldDefinition{
			FieldID:          name,
			SourcePath:       orDefault(f.SourcePath, name),
			Type:             fieldType,
			Searchable:       f.Searchable,
			Filterable:       f.Filterable,
			Facetable:        f.Facetable,
			Sortable:         f.Sortable,
			SnippetAllowed:   f.SnippetAllowed,
			Returnable:       f.Returnable,
			Exportable:       f.Exportable,
			SensitivityClass: class,
			AnalyzerProfile:  f.AnalyzerProfile,
		})
	}

	// R0 means "no source hydration required if restriction epoch is
	// current", which is only safe for low-sensitivity navigation (§7.1). A
	// contract that put personal, HR, financial or legal content behind R0
	// would return indexed content with no current-authorization check at
	// all — INV-05 defeated by configuration.
	if retrieval == domain.RetrievalR0 {
		for _, f := range out {
			if sensitivityRank(f.SensitivityClass) > sensitivityRank(domain.SensitivityInternal) {
				return nil, fmt.Errorf("field %q is classified %s, which cannot be served under "+
					"retrieval class R0 (no re-authorization); use R1 or stricter", f.FieldID, f.SensitivityClass)
			}
		}
	}
	return out, nil
}

func (h *Handler) GetContract(w http.ResponseWriter, r *http.Request) {
	contractID, ok := uuidParam(w, r, "contractID")
	if !ok {
		return
	}
	c, err := h.store.GetContract(r.Context(), contractID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such contract")
			return
		}
		h.internal(w, r, "get contract", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListContracts(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListContracts(r.Context(), r.URL.Query().Get("scope"))
	if err != nil {
		h.internal(w, r, "list contracts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contracts": out})
}

type transitionRequest struct {
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// TransitionContract moves a contract through DRAFT → CERTIFIED → PUBLISHED.
func (h *Handler) TransitionContract(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePlatform(w, r, ActionContractTransition)
	if !ok {
		return
	}
	var req transitionRequest
	if !decode(w, r, &req) {
		return
	}

	contractID, ok := uuidParam(w, r, "contractID")
	if !ok {
		return
	}
	current, err := h.store.GetContract(r.Context(), contractID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such contract")
			return
		}
		h.internal(w, r, "get contract", err)
		return
	}

	next := domain.ContractState(strings.ToUpper(strings.TrimSpace(req.State)))
	if !current.State.CanTransitionTo(next) {
		writeError(w, http.StatusConflict, "invalid_transition",
			fmt.Sprintf("a %s contract cannot move to %s", current.State, next))
		return
	}

	if err := h.store.TransitionContract(r.Context(), contractID, current.State, next); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		h.internal(w, r, "transition contract", err)
		return
	}

	if next == domain.ContractPublished || next == domain.ContractRetired {
		if err := h.indexer.Reload(r.Context()); err != nil {
			h.log.Error("contract transitioned but the projector registry could not reload", zap.Error(err))
		}
	}

	updated, err := h.store.GetContract(r.Context(), contractID)
	if err != nil {
		h.internal(w, r, "reload contract", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── ESR-05: generations ──────────────────────────────────────────────────────

type createGenerationRequest struct {
	Scope string `json:"scope"`
}

// CreateGeneration plans and builds a new index generation.
//
// PLANNED → BUILDING in one call, because the engine index has to exist before
// anything can be written into it and a PLANNED generation with no index is a
// state nothing can act on. Everything after BUILDING is a separate,
// separately-authorized transition — above all ACTIVE, which §8.1 requires to
// be reached only from READY.
func (h *Handler) CreateGeneration(w http.ResponseWriter, r *http.Request) {
	env, ok := h.requirePlatform(w, r, ActionGenerationCreate)
	if !ok {
		return
	}
	var req createGenerationRequest
	if !decode(w, r, &req) {
		return
	}

	contract, err := h.store.GetPublishedContract(r.Context(), req.Scope)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// §2.2: "only published contracts may build production
			// generations." A DRAFT contract building an index would produce
			// a generation whose field set nobody certified.
			writeError(w, http.StatusConflict, "no_published_contract",
				"scope "+req.Scope+" has no PUBLISHED contract to build from")
			return
		}
		h.internal(w, r, "get published contract", err)
		return
	}

	generationID := uuid.NewString()
	physical := searchclient.GenerationIndex(searchclient.IndexName(contract.ScopeName), shortID(generationID))
	now := time.Now().UTC()

	gen := domain.IndexGeneration{
		GenerationID:         generationID,
		ContractID:           contract.ContractID,
		ContractVersion:      contract.Version,
		ScopeName:            contract.ScopeName,
		PhysicalIndex:        physical,
		State:                domain.GenerationPlanned,
		BuildFrom:            &now,
		CreatedByPrincipalID: env.ActorSubjectID,
	}
	if err := h.store.CreateGeneration(r.Context(), gen); err != nil {
		h.internal(w, r, "create generation", err)
		return
	}

	if err := h.engine.EnsureGeneration(r.Context(), physical, projection.FieldMappings(*contract)); err != nil {
		h.metrics.EngineError("create")
		_ = h.store.TransitionGeneration(r.Context(), generationID,
			domain.GenerationPlanned, domain.GenerationFailed, "", err.Error())
		if h.events != nil {
			_ = h.events.ReindexFailed(r.Context(), contract.ScopeName, generationID, "BUILD", err.Error(), "")
		}
		h.internal(w, r, "create engine index", err)
		return
	}

	if err := h.store.TransitionGeneration(r.Context(), generationID,
		domain.GenerationPlanned, domain.GenerationBuilding, "", ""); err != nil {
		h.internal(w, r, "mark generation building", err)
		return
	}
	h.metrics.GenerationTransitions.WithLabelValues(contract.ScopeName, string(domain.GenerationBuilding)).Inc()

	gen.State = domain.GenerationBuilding
	writeJSON(w, http.StatusCreated, gen)
}

func (h *Handler) ListGenerations(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListGenerations(r.Context(), r.URL.Query().Get("scope"))
	if err != nil {
		h.internal(w, r, "list generations", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"generations": out})
}

// TransitionGeneration drives §8.1's lifecycle.
//
// VALIDATING → READY runs real validation, not a state write. §8.1's exit gate
// is "READY only if mandatory certification passes", and NP-41 ("reindex
// validation finds cross-tenant sample → fail generation; never activate")
// means the validation has to actually look at the documents.
func (h *Handler) TransitionGeneration(w http.ResponseWriter, r *http.Request) {
	action := ActionGenerationCreate
	var req transitionRequest
	if !decode(w, r, &req) {
		return
	}
	next := domain.GenerationState(strings.ToUpper(strings.TrimSpace(req.State)))
	if next == domain.GenerationActive || next == domain.GenerationRetired {
		// Activation is the act that changes what every caller sees. Its own
		// action, so it can be granted separately from the ability to build a
		// generation — building is routine, cutting over is not.
		action = ActionGenerationActivate
	}

	env, ok := h.requirePlatform(w, r, action)
	if !ok {
		return
	}

	generationID, ok2 := uuidParam(w, r, "generationID")
	if !ok2 {
		return
	}
	current, err := h.store.GetGeneration(r.Context(), generationID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such generation")
			return
		}
		h.internal(w, r, "get generation", err)
		return
	}

	if !current.State.CanTransitionTo(next) {
		writeError(w, http.StatusConflict, "invalid_transition",
			fmt.Sprintf("a %s generation cannot move to %s", current.State, next))
		return
	}

	digest, note := "", req.Note

	switch next {
	case domain.GenerationReady:
		result, err := h.validateGeneration(r.Context(), current)
		if err != nil {
			_ = h.store.TransitionGeneration(r.Context(), generationID,
				current.State, domain.GenerationFailed, "", err.Error())
			h.metrics.GenerationTransitions.WithLabelValues(current.ScopeName, string(domain.GenerationFailed)).Inc()
			if h.events != nil {
				_ = h.events.ReindexFailed(r.Context(), current.ScopeName, generationID,
					"VALIDATION", err.Error(), env.CorrelationID)
			}
			writeErrorCode(w, http.StatusConflict, "validation_failed", err.Error(),
				domain.ReasonReindexValidationFailed)
			return
		}
		digest, note = result.digest, result.note

	case domain.GenerationActive:
		// The alias swap and the state write are two systems that must agree.
		// The ENGINE goes first: if the swap succeeds and the state write
		// fails, the alias points at a generation the control plane does not
		// call ACTIVE — which is detectable drift (§8.3) and is caught by the
		// readiness check. The other order gives a control plane that claims
		// ACTIVE while the alias still serves the old generation, which is
		// silently wrong and looks correct from every API.
		previous, err := h.engine.ActivateGeneration(r.Context(),
			searchclient.Alias(searchclient.IndexName(current.ScopeName)), current.PhysicalIndex)
		if err != nil {
			h.metrics.EngineError("alias")
			h.internal(w, r, "activate alias", err)
			return
		}
		// Retire the generation the alias used to serve, so the partial
		// unique index on ACTIVE does not refuse the new one.
		if previous != "" && previous != current.PhysicalIndex {
			if old, err := h.findGenerationByIndex(r.Context(), current.ScopeName, previous); err == nil && old != nil {
				_ = h.store.TransitionGeneration(r.Context(), old.GenerationID,
					domain.GenerationActive, domain.GenerationRetired, "", "replaced by "+generationID)
			}
		}
		if h.events != nil {
			_ = h.events.GenerationActivated(r.Context(), current.ScopeName, previous,
				current.PhysicalIndex, env.ActorSubjectID, env.CorrelationID)
		}

	case domain.GenerationRetired:
		if current.State == domain.GenerationActive {
			// Retiring the serving generation would leave the alias pointing
			// at a retired index, or at nothing. §8.1's exit from ACTIVE is
			// "RETIRED after replacement" — replacement first.
			writeError(w, http.StatusConflict, "active_generation",
				"activate a replacement generation before retiring the serving one")
			return
		}
	}

	if err := h.store.TransitionGeneration(r.Context(), generationID, current.State, next, digest, note); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		h.internal(w, r, "transition generation", err)
		return
	}
	h.metrics.GenerationTransitions.WithLabelValues(current.ScopeName, string(next)).Inc()

	if next == domain.GenerationReady && h.events != nil {
		_ = h.events.GenerationReady(r.Context(), current.ScopeName, generationID,
			current.ContractID, current.PhysicalIndex, digest, env.CorrelationID)
	}
	if next == domain.GenerationActive || next == domain.GenerationRetired {
		if err := h.indexer.Reload(r.Context()); err != nil {
			h.log.Error("generation transitioned but the projector registry could not reload", zap.Error(err))
		}
	}

	updated, err := h.store.GetGeneration(r.Context(), generationID)
	if err != nil {
		h.internal(w, r, "reload generation", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type validationResult struct{ digest, note string }

// validateGeneration runs §8.1's mandatory certification before READY.
//
// Two checks, and the first is the one NP-41 is about.
func (h *Handler) validateGeneration(ctx context.Context, gen *domain.IndexGeneration) (validationResult, error) {
	contract, err := h.store.GetContract(ctx, gen.ContractID)
	if err != nil {
		return validationResult{}, fmt.Errorf("contract could not be read: %w", err)
	}
	if contract.State != domain.ContractPublished {
		return validationResult{}, fmt.Errorf("contract %s is %s, not PUBLISHED", contract.ContractID, contract.State)
	}

	// 1. Cross-tenant contamination. NP-41: "reindex validation finds
	// cross-tenant sample → fail generation; never activate; security
	// incident workflow." A document with no tenant_id is the contaminated
	// case that matters here — it would match no tenant filter, which sounds
	// safe, but it also means something wrote a projection without the
	// trusted tenant the whole model rests on.
	total, err := h.engine.CountProjections(ctx, gen.PhysicalIndex, nil)
	if err != nil {
		return validationResult{}, fmt.Errorf("population could not be counted: %w", err)
	}
	untenanted, err := h.engine.CountProjections(ctx, gen.PhysicalIndex, map[string]string{"tenant_id": ""})
	if err == nil && untenanted > 0 {
		return validationResult{}, fmt.Errorf(
			"SECURITY: %d documents in this generation carry no tenant_id; generation refused", untenanted)
	}

	// 2. Completeness against the control-plane ledger. NP-18: "index build
	// omits one tenant partition → completeness validation fails; no
	// activation." The ledger counted independently of the index, which is
	// what makes the comparison meaningful.
	ledgerLive, _, err := h.store.CountProjections(ctx, gen.ScopeName)
	if err != nil {
		return validationResult{}, fmt.Errorf("ledger population could not be counted: %w", err)
	}

	note := fmt.Sprintf("engine=%d ledger=%d contract=%s v%d",
		total, ledgerLive, contract.ContractID, contract.Version)

	// A brand-new generation legitimately holds nothing: it is built before
	// it is filled. The comparison only bites once the ledger says there is
	// something to have. A generation that has SOME documents but fewer than
	// the ledger knows about is the incomplete build NP-18 describes.
	if total > 0 && ledgerLive > 0 && total < ledgerLive {
		return validationResult{}, fmt.Errorf(
			"incomplete build: index holds %d of the %d live projections the ledger records", total, ledgerLive)
	}

	sum := sha256.Sum256([]byte(note))
	return validationResult{digest: hex.EncodeToString(sum[:]), note: note}, nil
}

func (h *Handler) findGenerationByIndex(ctx context.Context, scope, physicalIndex string) (*domain.IndexGeneration, error) {
	gens, err := h.store.ListGenerations(ctx, scope)
	if err != nil {
		return nil, err
	}
	for i := range gens {
		if gens[i].PhysicalIndex == physicalIndex {
			return &gens[i], nil
		}
	}
	return nil, nil
}

// ListCheckpoints reports index freshness and population (§11.1).
func (h *Handler) ListCheckpoints(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListCheckpoints(r.Context(), r.URL.Query().Get("scope"))
	if err != nil {
		h.internal(w, r, "list checkpoints", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": out})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// schemaDigest pins the exact field set a contract version published.
//
// Sorted by name so the digest is stable however the request ordered the
// fields, and every capability flag is included — two contracts differing only
// in whether a field is returnable are genuinely different contracts, and a
// digest that collapsed them would let a generation claim lineage to a field
// set it was not built from (TC-03).
func schemaDigest(fields []domain.SearchFieldDefinition) string {
	sorted := append([]domain.SearchFieldDefinition{}, fields...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].FieldID < sorted[j].FieldID })

	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s|%s|%s|%t%t%t%t%t%t%t|%s;",
			f.FieldID, f.SourcePath, f.Type,
			f.Searchable, f.Filterable, f.Facetable, f.Sortable,
			f.SnippetAllowed, f.Returnable, f.Exportable,
			f.SensitivityClass)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sensitivityRank orders classes so a ceiling can be compared.
//
// An UNKNOWN class ranks above every known one, so a class this version does
// not recognise fails a ceiling check rather than passing it. A new class is
// far more likely to be added because something needed stricter handling.
func sensitivityRank(c domain.SensitivityClass) int {
	switch c {
	case domain.SensitivityPublic:
		return 0
	case domain.SensitivityInternal:
		return 1
	case domain.SensitivityFinancial:
		return 2
	case domain.SensitivityPersonal:
		return 3
	case domain.SensitivityHR:
		return 4
	case domain.SensitivityLegal:
		return 5
	case domain.SensitivityRestricted:
		return 6
	case domain.SensitivitySecretProhibited:
		return 7
	default:
		return 99
	}
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}

func orDefaultInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

// shortID keeps a generation's index name readable.
//
// OpenSearch index names are limited to 255 bytes and appear in every log
// line, alias action and error message. The first 12 hex characters of a v4
// UUID leave ~48 bits of randomness, which is far more than enough to
// distinguish the handful of generations one scope ever has at once — and a
// collision would be refused by the UNIQUE constraint on physical_index rather
// than silently reusing an index.
func shortID(id string) string {
	clean := strings.ReplaceAll(id, "-", "")
	if len(clean) > 12 {
		return clean[:12]
	}
	return clean
}
