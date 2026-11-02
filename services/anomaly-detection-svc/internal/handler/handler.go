// Package handler is the HTTP layer of anomaly-detection-svc.
//
// Every handler follows the same order, so a reader can predict where a
// given failure is produced:
//
//  1. readScope            tenant + principal from headers        → 401
//  2. pathID               "<prefix><uuid>" path parameter        → 400
//  3. requireIfMatch       mutations of existing aggregates       → 428 / 412
//  4. requireIdempotencyKey every write                           → 400
//  5. decodeBody           strict JSON, tenant_id must agree      → 400 / 403
//  6. validate             domain rules on the request            → 422
//  7. authorize            authorization-svc, fail closed         → 403 / 503
//  8. store call           state machine, SoD, idempotency, RLS   → 404/409/412/403
//  9. respond              JSON (+ ETag, Location, Idempotent-Replayed)
//
// See ROUTES.md for the route table and the full code path.
package handler

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/anomaly-detection-svc/internal/store"
)

// Actions sent to authorization-svc. Each is purpose-scoped: reading,
// detecting, reviewing, closing and publishing models are separate
// permissions so they can be granted to different roles.
const (
	ActionRuleCreate         = "RULE_CREATE"
	ActionAnomalyRead        = "ANOMALY_READ"
	ActionAnomalyDetect      = "ANOMALY_DETECT"
	ActionAnomalyAck         = "ANOMALY_ACK"
	ActionAnomalyDisposition = "ANOMALY_DISPOSITION"
	ActionModelManage        = "ANOMALY_MODEL_MANAGE"
	ActionGovernedRead       = "ANOMALY_GOVERNED_READ"
)

// Authorizer is the slice of the authorization-svc client handlers use
// (*authz.Client in production, a fake in tests).
type Authorizer interface {
	CheckAllowed(ctx context.Context, principalID, scopeID, action string) error
}

// Handler holds the dependencies of every route.
type Handler struct {
	store  store.Store
	authz  Authorizer
	logger *zap.Logger
}

func New(st store.Store, authzClient Authorizer, logger *zap.Logger) *Handler {
	return &Handler{store: st, authz: authzClient, logger: logger}
}

// RegisterRoutes mounts every business route. Patterns are flat (no
// sub-routers) so each line here is exactly the route a client calls; the
// same list is in ROUTES.md and in the e2e suite's ROUTES.
func RegisterRoutes(r chi.Router, h *Handler) {
	// Rule-engine surface.
	r.Post("/v1/anomalies/rules", h.CreateRule)
	r.Get("/v1/anomalies/rules", h.ListRules)
	r.Get("/v1/anomalies/rules/{ruleID}", h.GetRule)
	r.Post("/v1/anomalies/detect", h.DetectAnomaly)
	r.Get("/v1/anomalies", h.ListAnomalies)
	r.Get("/v1/anomalies/{anomalyID}", h.GetAnomaly)
	r.Post("/v1/anomalies/{anomalyID}/status", h.UpdateAnomalyStatus)

	// AI-04 governed advisory layer: models.
	r.Post("/v1/anomaly-governance/models", h.RegisterAnomalyModel)
	r.Get("/v1/anomaly-governance/models", h.ListAnomalyModels)
	r.Get("/v1/anomaly-governance/models/{modelID}", h.GetAnomalyModel)
	r.Put("/v1/anomaly-governance/models/{modelID}", h.UpdateAnomalyModel)
	r.Get("/v1/anomaly-governance/models/{modelID}/drift-metrics", h.GetModelDriftMetrics)

	// AI-04: detection runs.
	r.Post("/v1/anomaly-governance/runs", h.RunDetection)
	r.Get("/v1/anomaly-governance/runs/{runID}", h.GetDetectionRun)
	r.Get("/v1/anomaly-governance/runs/{runID}/signals", h.GetSignalsByRun)

	// AI-04: signals and their review.
	r.Get("/v1/anomaly-governance/signals", h.ListSignals)
	r.Get("/v1/anomaly-governance/signals/{signalID}", h.GetGovernedSignal)
	r.Post("/v1/anomaly-governance/signals/{signalID}:acknowledge", h.AcknowledgeSignal)
	r.Post("/v1/anomaly-governance/signals/{signalID}:escalate", h.EscalateForReview)
	r.Post("/v1/anomaly-governance/signals/{signalID}:close", h.CloseSignal)
	r.Get("/v1/anomaly-governance/signals/{signalID}/disposition", h.GetDisposition)

	// AI-04: dispositions.
	r.Get("/v1/anomaly-governance/dispositions", h.ListDispositions)
}

// authorize asks authorization-svc whether the caller may perform action in
// its scope. Denied → 403; unreachable → 503. Never allows on error.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, scope requestScope, action string) bool {
	err := h.authz.CheckAllowed(r.Context(), scope.PrincipalID, scope.authzScope(), action)
	if err != nil {
		h.writeError(w, r, err)
		return false
	}
	return true
}
