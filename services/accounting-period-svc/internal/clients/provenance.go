// Package clients holds the HTTP implementations of the upstream contracts the
// service layer depends on (service.ProvenanceVerifier, service.CalendarClient).
package clients

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

const workloadID = "accounting-period-svc"

// HTTPProvenance verifies ACC-14 evidence against financial-close-svc.
//
// !!! THE ENDPOINT THIS CALLS DOES NOT EXIST YET. !!!
// GET {FinancialCloseURL}/v1/close/workflow-refs/{ref} is built in the later
// GL / financial-close cutover step. Until then financial-close-svc answers
// 404, every close/reopen command is rejected with SOURCE_UNVERIFIED, and that
// is the intended fail-closed behaviour: no evidence, no state change.
//
// Expected 200 body:
//
//	{"workflow_ref","period_key","legal_entity_id","command","status":"APPROVED","control_snapshot_ref"}
//
// Mapping (nothing here ever fails open):
//
//	network error / timeout / 5xx / 429 -> DEPENDENCY_UNAVAILABLE (503)
//	any other non-200 (404, 403, ...)   -> SOURCE_UNVERIFIED (422)
//	200 but status != APPROVED, or period_key / legal_entity_id / command /
//	  workflow_ref / control_snapshot_ref differs from the request -> SOURCE_UNVERIFIED
type HTTPProvenance struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPProvenance builds the verifier with a short timeout.
func NewHTTPProvenance(baseURL string) *HTTPProvenance {
	return &HTTPProvenance{BaseURL: baseURL, Client: &http.Client{Timeout: 3 * time.Second}}
}

var _ service.ProvenanceVerifier = (*HTTPProvenance)(nil)

type workflowRefResponse struct {
	WorkflowRef        string `json:"workflow_ref"`
	PeriodKey          string `json:"period_key"`
	LegalEntityID      string `json:"legal_entity_id"`
	Command            string `json:"command"`
	Status             string `json:"status"`
	ControlSnapshotRef string `json:"control_snapshot_ref"`
}

// Verify implements service.ProvenanceVerifier.
func (v *HTTPProvenance) Verify(ctx context.Context, r service.ProvenanceRequest) error {
	u := v.BaseURL + "/v1/close/workflow-refs/" + url.PathEscape(r.Acc14WorkflowRef)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return domain.Errf(domain.CodeDependencyUnavailable, "cannot build ACC-14 verification request")
	}
	req.Header.Set("X-Tenant-Id", r.TenantID)
	req.Header.Set("X-Workload-Id", workloadID)
	req.Header.Set("Accept", "application/json")

	resp, err := v.Client.Do(req)
	if err != nil {
		return domain.Errf(domain.CodeDependencyUnavailable, "ACC-14 (financial-close-svc) unreachable; failing closed")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return domain.Errf(domain.CodeDependencyUnavailable, "ACC-14 (financial-close-svc) answered %d; failing closed", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return domain.Errf(domain.CodeSourceUnverified, "ACC-14 does not recognise workflow ref %q (HTTP %d)", r.Acc14WorkflowRef, resp.StatusCode)
	}
	var body workflowRefResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return domain.Errf(domain.CodeSourceUnverified, "ACC-14 answer for %q is not valid JSON", r.Acc14WorkflowRef)
	}
	switch {
	case body.Status != "APPROVED":
		return domain.Errf(domain.CodeSourceUnverified, "ACC-14 workflow %q is %q, not APPROVED", r.Acc14WorkflowRef, body.Status)
	case body.WorkflowRef != r.Acc14WorkflowRef:
		return domain.Errf(domain.CodeSourceUnverified, "ACC-14 answered for a different workflow ref")
	case body.PeriodKey != r.PeriodKey || body.LegalEntityID != r.LegalEntityID:
		return domain.Errf(domain.CodeSourceUnverified, "ACC-14 workflow %q was approved for a different period or legal entity", r.Acc14WorkflowRef)
	case body.Command != string(r.Command):
		return domain.Errf(domain.CodeSourceUnverified, "ACC-14 workflow %q was approved for command %q, not %q", r.Acc14WorkflowRef, body.Command, r.Command)
	case body.ControlSnapshotRef != r.ControlSnapshotRef:
		return domain.Errf(domain.CodeSourceUnverified, "control_snapshot_ref does not match the one approved in ACC-14 workflow %q", r.Acc14WorkflowRef)
	}
	return nil
}
