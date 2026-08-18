// Package carta calls carta-svc to get a continuous session-risk assessment
// for a request that has already passed JWT verification.
//
// Doc 05 §3.11: "Authentication is not a one-time event. The platform must
// continuously reassess trust posture during a session." Verifying a JWT's
// signature only proves the token was validly issued at some point in the
// past — it says nothing about whether the request carrying it right now
// looks risky. This client is what makes that ongoing reassessment real
// instead of aspirational: it runs on every gated request, not just at
// login.
//
// Two of carta-svc's five scoring inputs (device trust level, known-location
// flag) have no real data source anywhere in this platform yet — there is no
// device-fingerprinting or geo-IP/known-locations service to ask. Rather
// than fabricate a signal with nothing real behind it, this client passes
// explicit neutral defaults for those two (see the doc comment on
// neutralDeviceTrust/assumeKnownLocation below) and relies on the three
// inputs that ARE real: principal ID, the actual client IP, and the actual
// time of day. The wiring is genuine and load-bearing for those three; the
// other two are an honestly-labeled placeholder pending real posture data,
// not a hidden gap.
package carta

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Decision mirrors carta-svc's domain.Decision.
type Decision string

const (
	DecisionAllow     Decision = "ALLOW"
	DecisionStepUpMFA Decision = "STEP_UP_MFA"
	DecisionIsolate   Decision = "ISOLATE"
	DecisionDeny      Decision = "DENY"

	// neutralDeviceTrust is the score used when no real device-posture
	// signal exists: exactly the midpoint, so it neither raises nor lowers
	// risk relative to carta-svc's own <50-is-untrusted threshold.
	neutralDeviceTrust = 50
	// assumeKnownLocation is true because we have no real known-locations
	// registry to check against, and defaulting to "unknown" would flag
	// every single request platform-wide on this factor alone, which is a
	// false signal, not a conservative one.
	assumeKnownLocation = true
)

// Assessment is the subset of carta-svc's CartaAssessment this caller acts
// on.
type Assessment struct {
	Decision    Decision `json:"decision"`
	RiskLevel   string   `json:"risk_level"`
	TrustScore  float64  `json:"trust_score"`
	RiskFactors []string `json:"risk_factors"`
}

type evaluateRequest struct {
	LegalEntityID string      `json:"legal_entity_id"`
	Context       evalContext `json:"context"`
}

type evalContext struct {
	SubjectID           string `json:"subject_id"`
	SubjectType         string `json:"subject_type"`
	DeviceTrustLevel    int    `json:"device_trust_level"`
	IPAddress           string `json:"ip_address"`
	IsKnownLocation     bool   `json:"is_known_location"`
	ResourceSensitivity string `json:"resource_sensitivity"`
	ActionRequested     string `json:"action_requested"`
	TimeOfDayHour       int    `json:"time_of_day_hour"`
}

// Client calls carta-svc. A nil/zero-value Client (baseURL == "") makes
// Evaluate a no-op that returns (nil, nil) — risk scoring is an additive
// safety layer, not a hard dependency every deployment must stand up before
// authentication works at all.
type Client struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func New(baseURL string, log *zap.Logger) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

// Evaluate asks carta-svc to score this request. principalID, tenantID, and
// legalEntityID come from the already-verified JWT; ipAddress and
// resourceSensitivity come from the actual request being forwarded.
//
// A nil Assessment (with nil error) means risk scoring was skipped —
// CARTA_SERVICE_URL unset, or carta-svc unreachable. This is a deliberate
// fail-OPEN for the scoring call itself: CARTA is an additive risk signal on
// top of JWT verification, which has already happened and already fails
// closed on its own. Losing the risk-scoring signal degrades this request to
// "scored the way every request was scored before this feature existed,"
// not to an unauthenticated pass-through.
func (c *Client) Evaluate(ctx context.Context, principalID, tenantID, legalEntityID, ipAddress, actionRequested, resourceSensitivity string) *Assessment {
	if c == nil || c.baseURL == "" {
		return nil
	}

	body, err := json.Marshal(evaluateRequest{
		LegalEntityID: legalEntityID,
		Context: evalContext{
			SubjectID:           principalID,
			SubjectType:         "USER",
			DeviceTrustLevel:    neutralDeviceTrust,
			IPAddress:           ipAddress,
			IsKnownLocation:     assumeKnownLocation,
			ResourceSensitivity: resourceSensitivity,
			ActionRequested:     actionRequested,
			TimeOfDayHour:       time.Now().UTC().Hour(),
		},
	})
	if err != nil {
		c.log.Warn("carta: failed to marshal evaluate request", zap.Error(err))
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/carta/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("carta-svc unreachable — proceeding without a risk assessment", zap.Error(err))
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		c.log.Warn("carta-svc returned an unexpected status", zap.Int("status", resp.StatusCode))
		return nil
	}

	var out Assessment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.log.Warn("carta-svc response unreadable", zap.Error(err))
		return nil
	}
	return &out
}
