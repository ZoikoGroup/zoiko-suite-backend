package handler

import (
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/reconciliation-engine-svc/internal/source"
)

// POST /dqc/reconciliations/{definitionID}/runs (ZS-SVC-Z-001 section 11) is StartRun with the definition named
// in the path: the same authorization against the definition's entity, the same refusals, the same recording of
// a run whose population cannot be proven.

const dqcRunPath = "/v1/reconciliation/dqc/reconciliations/rdf_1/runs"

func TestDQCRunRoute_IsStartRunForTheNamedDefinition(t *testing.T) {
	for name, body := range map[string]string{
		"definition only in the path": `{"period_id":"2026-09"}`,
		"body agrees with the path":   `{"definition_id":"rdf_1","period_id":"2026-09"}`,
	} {
		az := &recordingAuthz{}
		st := &fakeStore{def: definition()}
		h := NewHandler(st, az, failingFetcher{err: source.ErrSourceUnavailable}, zap.NewNop())
		w := serve(h, http.MethodPost, dqcRunPath, body)
		// The source is unavailable, so the run is recorded as Failed/INDETERMINATE: proof that the handler reached the
		// definition named in the path, authorized against its entity, and went through the same path as /runs.
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"rcr_failed"`) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		if len(az.entities) != 1 || az.entities[0] != entityA {
			t.Fatalf("%s: authorized against %v, want the definition's entity", name, az.entities)
		}
		if len(st.failures) != 1 {
			t.Fatalf("%s: %d failed runs recorded, want one", name, len(st.failures))
		}
	}
}

func TestDQCRunRoute_ABodyThatNamesAnotherDefinitionIsRefused(t *testing.T) {
	az := &recordingAuthz{}
	st := &fakeStore{def: definition()}
	h := NewHandler(st, az, failingFetcher{err: source.ErrSourceUnavailable}, zap.NewNop())
	w := serve(h, http.MethodPost, dqcRunPath, `{"definition_id":"rdf_OTHER","period_id":"2026-09"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a contradiction: %d %s", w.Code, w.Body)
	}
	if len(az.entities) != 0 || st.startCalls != 0 || len(st.failures) != 0 {
		t.Fatalf("a contradictory request reached authorization (%v), the store (%d) or recorded a run (%d)", az.entities, st.startCalls, len(st.failures))
	}
}

func TestDQCRunRoute_RefusesUnapprovedRulesLikeStartRun(t *testing.T) {
	def := definition()
	def.Version, def.Materiality, def.ExceptionPolicy, def.CertificationProfile = 0, nil, nil, nil
	calls := 0
	st := &fakeStore{def: def}
	w := serve(NewHandler(st, &recordingAuthz{}, failingFetcher{err: source.ErrSourceUnavailable, calls: &calls}, zap.NewNop()),
		http.MethodPost, dqcRunPath, `{"period_id":"2026-09"}`)
	if w.Code != http.StatusConflict || calls != 0 || st.startCalls != 0 {
		t.Fatalf("unapproved rules: %d %s, source reads %d", w.Code, w.Body, calls)
	}
}
