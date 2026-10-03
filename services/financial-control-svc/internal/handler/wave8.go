package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/store"
)

// ActionExport is the authority to export a run's evidence bundle for external reliance.
const ActionExport = "FINCTRL_EVIDENCE_EXPORT"

// ExceptionExport is one exception with its full append-only history.
type ExceptionExport struct {
	Exception   domain.ControlException      `json:"exception"`
	Transitions []domain.ExceptionTransition `json:"transitions"`
}

// RunEvidenceBundle is a self-verifying export of everything a reviewer or external
// auditor needs to re-perform one run without access to the platform: the run and its
// state history, the pinned rule version, frozen population summaries (row counts, totals,
// hashes, exclusions), every exception with authority/evidence/root-cause history, and the
// sealed evidence package with its digest re-verified at export time.
//
// Manifest.Digest is the canonical sha256 of the bundle with the manifest removed, so a
// recipient can recompute it and detect any alteration in transit.
type RunEvidenceBundle struct {
	Run           *domain.ControlRun          `json:"run"`
	RunTransition []domain.Transition         `json:"run_transitions"`
	Definition    *domain.ControlDefinition   `json:"control_definition"`
	RuleVersion   *domain.ControlRuleVersion  `json:"rule_version"`
	Populations   []domain.PopulationSnapshot `json:"population_snapshots"`
	Exceptions    []ExceptionExport           `json:"exceptions"`
	Evidence      *domain.EvidencePackage     `json:"evidence_package"`
	Manifest      BundleManifest              `json:"manifest"`
}

type BundleManifest struct {
	Format     string    `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	ExportedBy string    `json:"exported_by"`
	Algorithm  string    `json:"algorithm"`
	Digest     string    `json:"digest"`
	// EvidenceVerified is false if the stored evidence package no longer matches its seal.
	EvidenceVerified bool `json:"evidence_integrity_verified"`
}

const bundleFormat = "zoiko.financial-control.run-evidence/v1"

// ExportRunEvidence — GET /controls/v1/runs/{run_id}/evidence-export (Wave 8: evidence
// export, external-audit support). Read-only; authorised against the run's own entity
// with FINCTRL_EVIDENCE_EXPORT, a separate action from FINCTRL_READ because an export
// leaves the platform.
func (h *Handler) ExportRunEvidence(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "run_id"))
	if err != nil {
		h.writeErr(w, "export evidence", err)
		return
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, ActionExport) {
		return
	}
	ctx := r.Context()
	fail := func(err error) { h.writeErr(w, "export evidence", err) }

	b := RunEvidenceBundle{Run: run}
	if b.RunTransition, err = h.store.ListTransitions(ctx, tenantID, run.RunID); err != nil {
		fail(err)
		return
	}
	if b.Definition, err = h.store.GetDefinition(ctx, tenantID, run.ControlDefinitionID); err != nil {
		fail(err)
		return
	}
	versions, err := h.store.ListRuleVersions(ctx, tenantID, run.ControlDefinitionID)
	if err != nil {
		fail(err)
		return
	}
	for i := range versions {
		if versions[i].RuleVersion == run.RuleVersion {
			b.RuleVersion = &versions[i]
		}
	}
	if b.Populations, err = h.store.ListPopulationSnapshots(ctx, tenantID, run.RunID); err != nil {
		fail(err)
		return
	}

	b.Exceptions = []ExceptionExport{}
	after := ""
	for {
		page, err := h.store.ListExceptions(ctx, tenantID, run.RunID, store.ListExceptionsFilter{Limit: 200, AfterID: after})
		if err != nil {
			fail(err)
			return
		}
		for _, e := range page {
			tr, err := h.store.ListExceptionTransitions(ctx, tenantID, e.ExceptionID)
			if err != nil {
				fail(err)
				return
			}
			b.Exceptions = append(b.Exceptions, ExceptionExport{Exception: e, Transitions: nonNil(tr)})
		}
		if len(page) < 200 {
			break
		}
		after = page[len(page)-1].ExceptionID
	}

	if b.Evidence, err = h.store.GetLatestEvidence(ctx, tenantID, run.RunID); err != nil && err != domain.ErrNotFound {
		fail(err)
		return
	}
	verified := b.Evidence != nil && b.Evidence.Verified != nil && *b.Evidence.Verified

	// Digest everything except the manifest itself.
	raw, err := json.Marshal(b)
	if err != nil {
		fail(err)
		return
	}
	digest, err := domain.CanonicalDigest(raw)
	if err != nil {
		fail(err)
		return
	}
	b.Manifest = BundleManifest{Format: bundleFormat, ExportedAt: time.Now().UTC(), ExportedBy: principal,
		Algorithm: "sha256-canonical-json", Digest: digest, EvidenceVerified: verified}
	w.Header().Set("Content-Disposition", `attachment; filename="control-run-`+run.RunID+`-evidence.json"`)
	writeJSON(w, http.StatusOK, b)
}

// Monitoring — GET /controls/v1/monitoring/metrics?legal_entity_id=&period_id= (Wave 7:
// continuous monitoring, recurrence and root-cause metrics). period_id is optional; without
// it the figures cover every period of the entity. This is the data feed for the
// financial-control command center; it never changes any control state.
func (h *Handler) Monitoring(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	entity := r.URL.Query().Get("legal_entity_id")
	period := r.URL.Query().Get("period_id")
	if !uuidRe.MatchString(entity) {
		writeError(w, http.StatusBadRequest, "invalid_request", "legal_entity_id must be a UUID")
		return
	}
	if len(period) > 32 {
		writeError(w, http.StatusBadRequest, "invalid_request", "period_id is at most 32 characters")
		return
	}
	if !h.authorize(w, r, principal, entity, ActionRead) {
		return
	}
	m, err := h.store.Monitoring(r.Context(), tenantID, entity, period, time.Now().UTC())
	if err != nil {
		h.writeErr(w, "monitoring", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
