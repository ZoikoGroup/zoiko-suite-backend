package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
)

// SoDClient checks segregation-of-duties conflicts via authorization-svc's
// POST /v1/sod/validate. It is called BEFORE provisioning a bundle so a
// conflicted role definition never reaches the enforcement plane.
//
// ── THE CONTRACT, AND WHAT THE PREVIOUS CLIENT SENT INSTEAD ─────────────────
//
// authorization-svc takes {candidate_actions, principal_id?, legal_entity_id?,
// tenant_id} and ALWAYS answers a verdict with 200: {conflict_free, conflicts}.
// It has no 409. The client this replaces posted {role_code, bundle_code,
// permitted_actions, principal_id, legal_entity_id, ...}, which carries no
// candidate_actions, so every call was answered 400 missing_field. Its only
// conflict signal was a 409 that cannot occur, every other non-2xx was a
// generic error, and the handler turned every error into 403 sod_conflict.
// Two results followed:
//
//  1. Every bundle create and every action edit was refused 403 sod_conflict
//     (as was every role create, which made a meaningless SoD call with no
//     actions at all). The service could not provision anything.
//  2. Had the body been right, a real conflict (200, conflict_free:false)
//     would have been read as a pass. The control failed open by design.
//
// This client sends the documented body, reads the verdict strictly, and
// fails closed: anything other than a 200 with conflict_free present is
// domain.ErrSoDUnavailable, never a pass and never a conflict.
type SoDClient struct {
	baseURL string
	http    *http.Client
}

func NewSoDClient(baseURL string) *SoDClient {
	return &SoDClient{baseURL: baseURL, http: &http.Client{Timeout: 3 * time.Second}}
}

type sodValidateRequest struct {
	CandidateActions []string `json:"candidate_actions"`
	// PrincipalID / LegalEntityID turn the question into "would THIS
	// principal, with everything they already hold in this entity, conflict
	// once they also hold these". Used for assignments; a role definition
	// asks the candidate-only form.
	PrincipalID   string `json:"principal_id,omitempty"`
	LegalEntityID string `json:"legal_entity_id,omitempty"`
	TenantID      string `json:"tenant_id"`
}

type sodValidateResponse struct {
	// A pointer, so a 200 without a verdict (a proxy page, a renamed field)
	// is told apart from conflict_free:false and from true.
	ConflictFree *bool                `json:"conflict_free"`
	Conflicts    []domain.SoDConflict `json:"conflicts"`
}

// CheckConflict returns nil when the candidate set is conflict-free, a
// *domain.SoDConflictError (errors.Is ErrSoDConflict) when it is not, and an
// error wrapping domain.ErrSoDUnavailable when no verdict could be read.
func (c *SoDClient) CheckConflict(ctx context.Context, req domain.SoDCheckRequest) error {
	body, err := json.Marshal(sodValidateRequest{
		CandidateActions: req.CandidateActions,
		PrincipalID:      req.SubjectPrincipalID,
		LegalEntityID:    req.LegalEntityID,
		TenantID:         req.TenantID,
	})
	if err != nil {
		return fmt.Errorf("%w: marshal: %v", domain.ErrSoDUnavailable, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/sod/validate", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrSoDUnavailable, err)
	}
	correlationID := req.CorrelationID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Principal-Id", req.CallerID)
	httpReq.Header.Set("X-Tenant-Id", req.TenantID)
	httpReq.Header.Set("X-Correlation-ID", correlationID)
	httpReq.Header.Set("X-Request-Id", uuid.NewString())
	httpReq.Header.Set("X-Source-Channel", "system")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%w: unreachable: %v", domain.ErrSoDUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%w: authorization-svc returned %d: %s", domain.ErrSoDUnavailable, resp.StatusCode, string(detail))
	}
	var verdict sodValidateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&verdict); err != nil {
		return fmt.Errorf("%w: unreadable verdict: %v", domain.ErrSoDUnavailable, err)
	}
	if verdict.ConflictFree == nil {
		return fmt.Errorf("%w: response carried no conflict_free verdict", domain.ErrSoDUnavailable)
	}
	if !*verdict.ConflictFree {
		return &domain.SoDConflictError{Conflicts: verdict.Conflicts}
	}
	return nil
}
