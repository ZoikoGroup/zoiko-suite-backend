package httpapi

import (
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
)

// statusWriter remembers the status code written, for diagnostics.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// turnDiag is what a completed turn reports: never message text.
type turnDiag struct {
	status, intent, reason string
	pipeline               orchestrator.Diag
}

// edgeErrors names a refused request by its HTTP status (EDGE stage).
var edgeErrors = map[int]string{
	http.StatusBadRequest: "INVALID_INPUT", http.StatusUnauthorized: "SESSION_EXPIRED",
	http.StatusForbidden: "ORIGIN_OR_SCOPE_DENIED", http.StatusConflict: "TURN_CONFLICT",
	http.StatusRequestEntityTooLarge: "MESSAGE_TOO_LONG", http.StatusUnsupportedMediaType: "UNSUPPORTED_MEDIA_TYPE",
	http.StatusUnprocessableEntity: "IDEMPOTENCY_OR_VALIDATION", http.StatusTooManyRequests: "RATE_LIMITED",
	http.StatusServiceUnavailable: "KNOWLEDGE_UNAVAILABLE", http.StatusGatewayTimeout: "MODEL_TIMEOUT",
}

// logTurnDiag writes the per-turn diagnostics line in dev/test only.
func (s *Server) logTurnDiag(requestID string, httpStatus int, d turnDiag) {
	if s.cfg.Environment != "dev" && s.cfg.Environment != "test" {
		return
	}
	stage, reason := contract.StageFor(d.reason), d.reason
	if httpStatus >= 400 || d.status == "" {
		stage, reason = contract.StageEdge, edgeErrors[httpStatus]
		if reason == "" {
			reason = "HTTP_" + strconv.Itoa(httpStatus)
		}
	}
	p := d.pipeline
	s.log.Info("turn diagnostics", zap.String("request_id", requestID), zap.Int("http_status", httpStatus),
		zap.String("status", d.status), zap.String("intent", d.intent), zap.String("stage", stage), zap.String("reason", reason),
		zap.Any("slots", p.Slots), zap.String("claim_verdict", p.ClaimVerdict), zap.String("retrieval_mode", p.RetrievalMode),
		zap.Int("candidates_before_acl", p.CandidatesBeforeACL), zap.Int("candidates_after_acl", p.CandidatesAfterACL),
		zap.Float64("top_score", p.TopScore), zap.Strings("evidence_ids", p.EvidenceIDs))
}
