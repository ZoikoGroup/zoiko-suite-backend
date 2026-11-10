package handler

import (
	"strings"
	"time"
)

// DecisionMetrics is the §26 decision telemetry the handler reports to
// (*telemetry.AuthzMetrics in main). Nil records nothing, which is what the
// tests and any handler built without SetMetrics get.
type DecisionMetrics interface {
	ObserveDecision(decision, outcome, layer, action string)
	ObserveEvaluation(d time.Duration)
	DecisionRecordFailed()
}

// SetMetrics wires the decision telemetry.
func (h *Handler) SetMetrics(m DecisionMetrics) { h.metrics = m }

// decisionLayer is the bounded label for "which layer decided": the basis up
// to its first ':' or '=' — rbac / delegated / pam / break_glass / support on a
// PERMIT, and principal_status / entity_status / workload / no_grant / sod /
// abac / delegation_limit / authority_limit / quorum / revalidation /
// assurance / state on a refusal. Anything else is "other", so a malformed
// basis cannot grow the label set.
func decisionLayer(basis string) string {
	layer := basis
	if i := strings.IndexAny(layer, ":="); i >= 0 {
		layer = layer[:i]
	}
	if knownLayers[layer] {
		return layer
	}
	return "other"
}

var knownLayers = map[string]bool{
	"rbac": true, "delegated": true, "pam": true, "break_glass": true, "support": true,
	"principal_status": true, "entity_status": true, "workload": true, "no_grant": true,
	"sod": true, "abac": true, "delegation_limit": true, "authority_limit": true,
	"quorum": true, "revalidation": true, "assurance": true, "state": true,
}

func (h *Handler) observeEvaluation(start time.Time) {
	if h.metrics != nil {
		h.metrics.ObserveEvaluation(time.Since(start))
	}
}

// observeRecorded counts a decision once its artefact is written — the same
// moment the PEP can rely on it — or the refusal when it could not be.
func (h *Handler) observeRecorded(in evalContext, res *evalResult, recorded bool) {
	if h.metrics == nil || res == nil {
		return
	}
	if !recorded {
		h.metrics.DecisionRecordFailed()
		return
	}
	h.metrics.ObserveDecision(res.Decision, res.Outcome, decisionLayer(res.Basis), in.ActionType)
}
