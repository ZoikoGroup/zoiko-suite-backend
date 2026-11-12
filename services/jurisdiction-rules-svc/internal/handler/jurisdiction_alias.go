package handler

// REF-01 (ZS-SVC-I-001 s4.11): AmendJurisdictionAlias, ResolveJurisdiction and
// MapExternalJurisdictionCode.
//
//	GET  /v1/jurisdiction-resolutions?code=&system=&type=&as_of=   ResolveJurisdiction (public read)
//	GET  /v1/jurisdiction-code-mappings?system=&code=&as_of=       MapExternalJurisdictionCode (public read)
//	GET  /v1/jurisdictions/{id}/aliases?as_of=                      the codes a jurisdiction is known by
//	POST /v1/admin/jurisdictions/{id}/aliases                       AmendJurisdictionAlias
//	POST /v1/admin/jurisdictions/{id}/aliases/{alias_id}/end        end a mapping
//
// "Unknown/retired codes fail explicit resolution ... never guess from free
// text." So a resolution answers one of four refusals, each with its own status
// and code, and never a near match:
//
//	404 unknown_jurisdiction_code   nothing is known by that code
//	404 code_not_mapped_at          the code is mapped, but not at as_of
//	410 jurisdiction_retired        it resolves to a retired jurisdiction (with the successor)
//	409 ambiguous_jurisdiction_code more than one jurisdiction answers to it (with the candidates)

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

type jurisdictionAliasStore interface {
	ResolveCode(ctx context.Context, q domain.ResolveQuery) (*domain.CodeResolution, error)
	ListJurisdictionAliases(ctx context.Context, jurisdictionID string, at *time.Time) ([]*domain.JurisdictionAlias, error)
	AddJurisdictionAliasWithQuerier(ctx context.Context, q store.Querier, p domain.AddAliasParams) (*domain.JurisdictionAlias, bool, *domain.Jurisdiction, error)
	EndJurisdictionAliasWithQuerier(ctx context.Context, q store.Querier, p domain.EndAliasParams) (*domain.JurisdictionAlias, bool, *domain.Jurisdiction, error)
}

// AmendAliasRequest is the body of POST /v1/admin/jurisdictions/{id}/aliases.
type AmendAliasRequest struct {
	AliasSystem      string     `json:"alias_system"`
	AliasCode        string     `json:"alias_code"`
	SourceStandard   string     `json:"source_standard"`
	SourceVersion    string     `json:"source_version"`
	MappingRationale string     `json:"mapping_rationale"`
	EvidenceRef      string     `json:"evidence_ref"`
	ValidFrom        time.Time  `json:"valid_from"`
	ValidTo          *time.Time `json:"valid_to"`
	ReplacesAliasID  string     `json:"replaces_alias_id"`
}

// EndAliasRequest is the body of POST /v1/admin/jurisdictions/{id}/aliases/{alias_id}/end.
type EndAliasRequest struct {
	ValidTo time.Time `json:"valid_to"`
	Reason  string    `json:"reason"`
}

func (h *Handler) aliasStore(w http.ResponseWriter, r *http.Request) (jurisdictionAliasStore, bool) {
	s, ok := h.store.(jurisdictionAliasStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "the store does not support jurisdiction aliases")
	}
	return s, ok
}

// writeAliasError answers the refusals the alias surface has.
func (h *Handler) writeAliasError(w http.ResponseWriter, r *http.Request, err error, correlationID string) {
	var (
		retired   *domain.RetiredError
		ambiguous *domain.AmbiguousError
		unmapped  *domain.AliasNotEffectiveError
	)
	switch {
	case errors.Is(err, domain.ErrUnknownJurisdictionCode):
		writeError(w, r, http.StatusNotFound, "unknown_jurisdiction_code", "no jurisdiction is known by that code")
	case errors.As(err, &unmapped):
		apicontract.WriteProblem(w, r, apicontract.Problem{
			Status: http.StatusNotFound, Code: "code_not_mapped_at", Detail: unmapped.Error(), CorrelationID: correlationID,
		})
	case errors.As(err, &retired):
		extra := map[string]any{"jurisdiction_id": retired.JurisdictionID, "jurisdiction_code": retired.Code}
		if retired.RetiredAt != nil {
			extra["retired_at"] = retired.RetiredAt
		}
		if retired.SupersededBy != nil {
			extra["superseded_by_jurisdiction_id"] = *retired.SupersededBy
		}
		if retired.Reason != nil {
			extra["retirement_reason"] = *retired.Reason
		}
		writeProblemWith(w, r, http.StatusGone, "jurisdiction_retired", retired.Error(), extra)
	case errors.As(err, &ambiguous):
		writeProblemWith(w, r, http.StatusConflict, "ambiguous_jurisdiction_code", ambiguous.Error(),
			map[string]any{"candidates": ambiguous.Candidates})
	case errors.Is(err, domain.ErrAliasConflict):
		writeError(w, r, http.StatusConflict, "alias_conflict", err.Error())
	case errors.Is(err, domain.ErrAliasNotFound):
		writeError(w, r, http.StatusNotFound, "alias_not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidAlias):
		writeError(w, r, http.StatusBadRequest, "invalid_alias", err.Error())
	default:
		// jurisdiction_not_found, not-effective, too-long, store outage.
		h.writeHistoryError(w, r, err, correlationID)
	}
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, requireSystem bool) {
	s, ok := h.aliasStore(w, r)
	if !ok {
		return
	}
	qs := r.URL.Query()
	code, system := strings.TrimSpace(qs.Get("code")), strings.TrimSpace(qs.Get("system"))
	if code == "" {
		writeMissingField(w, r, "code")
		return
	}
	if requireSystem && (system == "" || system == domain.SystemInternal) {
		writeMissingField(w, r, "system")
		return
	}
	if len(code) > 64 || len(system) > 64 {
		writeError(w, r, http.StatusBadRequest, "input_too_long", "code and system must be at most 64 characters")
		return
	}
	at, ok := asOfQuery(w, r)
	if !ok {
		return
	}
	out, err := s.ResolveCode(r.Context(), domain.ResolveQuery{
		System: system, Code: code, Type: strings.TrimSpace(qs.Get("type")), AsOf: at,
	})
	if err != nil {
		h.writeAliasError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ResolveJurisdiction handles GET /v1/jurisdiction-resolutions. With no system
// the code is a jurisdiction's own code; with one it is that system's code.
func (h *Handler) ResolveJurisdiction(w http.ResponseWriter, r *http.Request) { h.resolve(w, r, false) }

// MapExternalJurisdictionCode handles GET /v1/jurisdiction-code-mappings: the
// same resolution, with the code system required, and the mapping's source and
// rationale in the answer.
func (h *Handler) MapExternalJurisdictionCode(w http.ResponseWriter, r *http.Request) {
	h.resolve(w, r, true)
}

// ListJurisdictionAliases handles GET /v1/jurisdictions/{jurisdiction_id}/aliases.
func (h *Handler) ListJurisdictionAliases(w http.ResponseWriter, r *http.Request) {
	s, ok := h.aliasStore(w, r)
	if !ok {
		return
	}
	var at *time.Time
	if strings.TrimSpace(r.URL.Query().Get("as_of")) != "" {
		t, ok := asOfQuery(w, r)
		if !ok {
			return
		}
		at = &t
	}
	p, ok := h.beginList(w, r, nil, nil)
	if !ok {
		return
	}
	out, err := s.ListJurisdictionAliases(r.Context(), chi.URLParam(r, "jurisdiction_id"), at)
	if err != nil {
		h.writeAliasError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	finishList(h, w, r, p, pageOf(p, out))
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must be a JSON object")
		return false
	}
	return true
}

// publishUpdated emits jurisdiction.updated inside the transaction, if the
// publisher can.
func (h *Handler) publishUpdated(ctx context.Context, j *domain.Jurisdiction, c events.JurisdictionChange, correlationID string) error {
	if pub, ok := h.publisher.(events.JurisdictionUpdatePublisher); ok && j != nil {
		return pub.PublishJurisdictionUpdated(ctx, *j, c, correlationID)
	}
	return nil
}

// AmendJurisdictionAlias handles POST /v1/admin/jurisdictions/{jurisdiction_id}/aliases.
//
//	201 → the mapping was created
//	200 → an identical mapping already existed (idempotent replay; nothing emitted)
//	400 → invalid alias (system, code format, missing source or rationale, dates)
//	404 → the jurisdiction is unknown or retired
//	409 → the code already means something else during part of that interval
func (h *Handler) AmendJurisdictionAlias(w http.ResponseWriter, r *http.Request) {
	s, ok := h.aliasStore(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "alias_amend"); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	var req AmendAliasRequest
	if !decodeBody(w, r, &req) {
		return
	}

	var alias *domain.JurisdictionAlias
	var created bool
	err := h.store.WithTransaction(r.Context(), func(q store.Querier) error {
		ctx := events.WithBatch(r.Context())
		a, isNew, j, err := s.AddJurisdictionAliasWithQuerier(r.Context(), q, domain.AddAliasParams{
			JurisdictionID: chi.URLParam(r, "jurisdiction_id"), AliasSystem: req.AliasSystem, AliasCode: req.AliasCode,
			SourceStandard: req.SourceStandard, SourceVersion: req.SourceVersion, MappingRationale: req.MappingRationale,
			EvidenceRef: req.EvidenceRef, ValidFrom: req.ValidFrom, ValidTo: req.ValidTo,
			ReplacesAliasID: req.ReplacesAliasID, ActorID: principalID,
		})
		if err != nil {
			return err
		}
		alias, created = a, isNew
		if !isNew {
			return nil // a replay emits nothing
		}
		if err := h.publishUpdated(ctx, j, events.JurisdictionChange{Type: events.ChangeAliasAdded, Alias: a}, correlationID); err != nil {
			return err
		}
		return h.publisher.FlushPending(ctx, q)
	})
	if err != nil {
		h.writeAliasError(w, r, err, correlationID)
		return
	}
	h.log.Info("AmendJurisdictionAlias", zap.String("alias_id", alias.AliasID), zap.Bool("created", created),
		zap.String("principal_id", principalID), zap.String("correlation_id", correlationID))
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, alias)
}

// EndJurisdictionAlias handles POST /v1/admin/jurisdictions/{jurisdiction_id}/aliases/{alias_id}/end.
func (h *Handler) EndJurisdictionAlias(w http.ResponseWriter, r *http.Request) {
	s, ok := h.aliasStore(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "alias_end"); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	var req EndAliasRequest
	if !decodeBody(w, r, &req) {
		return
	}

	var alias *domain.JurisdictionAlias
	err := h.store.WithTransaction(r.Context(), func(q store.Querier) error {
		ctx := events.WithBatch(r.Context())
		a, changed, j, err := s.EndJurisdictionAliasWithQuerier(r.Context(), q, domain.EndAliasParams{
			AliasID: chi.URLParam(r, "alias_id"), JurisdictionID: chi.URLParam(r, "jurisdiction_id"),
			ValidTo: req.ValidTo, ActorID: principalID, Reason: req.Reason,
		})
		if err != nil {
			return err
		}
		alias = a
		if !changed {
			return nil
		}
		if err := h.publishUpdated(ctx, j, events.JurisdictionChange{Type: events.ChangeAliasEnded, Alias: a}, correlationID); err != nil {
			return err
		}
		return h.publisher.FlushPending(ctx, q)
	})
	if err != nil {
		h.writeAliasError(w, r, err, correlationID)
		return
	}
	writeJSON(w, http.StatusOK, alias)
}
