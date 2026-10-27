package release

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

// Artifact is one independently versioned, independently revertible part
// of a release (Doc 08 §01: app, prompt/policy, public KB, P1 claims).
type Artifact struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256,omitempty"`
}

// ReleaseRecord is the immutable release manifest (Doc 08 §06, Doc 11 §21).
// It records evidence; it never records an approval that was not given.
type ReleaseRecord struct {
	ReleaseID      string              `json:"release_id"`
	Scope          string              `json:"scope"`
	CreatedAt      string              `json:"created_at"`
	Artifacts      map[string]Artifact `json:"artifacts"`
	ModelProfile   string              `json:"model_profile"`
	SchemaContract string              `json:"schema_contract"`
	RolloutStage   string              `json:"rollout_stage"`
	AssuranceRuns  map[string]string   `json:"assurance_runs"`
	Approvals      map[string]string   `json:"approvals"`
	Decision       string              `json:"decision"`
	Limitations    []string            `json:"limitations"`
}

// ManifestInput is what the operator supplies.
type ManifestInput struct {
	ReleaseID      string
	GitCommit      string
	ImageDigest    string
	RolloutStage   string
	ModelProfile   string
	EvalReportPath string
	PolicyVersion  string
}

// BuildManifest reads the currently promoted kb/claims releases and the
// eval report, and returns the release record.
func BuildManifest(ctx context.Context, st store.Store, in ManifestInput) (ReleaseRecord, error) {
	rec := ReleaseRecord{
		ReleaseID: in.ReleaseID, Scope: "R0_PUBLIC_GUIDANCE", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Artifacts: map[string]Artifact{}, ModelProfile: in.ModelProfile, SchemaContract: "suivo-api-v1",
		RolloutStage: in.RolloutStage,
		AssuranceRuns: map[string]string{
			"functional": "NOT_RECORDED", "ai_eval": "NOT_EXECUTED", "security": "NOT_EXECUTED",
			"accessibility": "NOT_EXECUTED (widget not built)", "rollback_drill": "NOT_RECORDED",
		},
		Approvals: map[string]string{"product": "PENDING", "cto": "PENDING", "security": "PENDING", "dpo": "PENDING", "qa": "PENDING", "sre": "PENDING", "founder": "PENDING"},
		Decision:  "NO_GO_UNTIL_SIGNED",
		Limitations: []string{
			"All documents PROPOSED; no gate approved.",
			"Model profile is the fake provider: no approved vendor (DEC-05).",
			"Knowledge and claims are whatever releases are promoted; fixtures are TEST-ONLY.",
			"Numeric limits, TTLs and SLOs are unapproved dev defaults (DEC-07, DEC-08, D06-05).",
		},
	}
	if in.GitCommit == "" || in.ReleaseID == "" {
		return rec, fmt.Errorf("release id and git commit are required")
	}
	rec.Artifacts["application"] = Artifact{Version: in.GitCommit, SHA256: in.ImageDigest}
	rec.Artifacts["prompt_policy"] = Artifact{Version: in.PolicyVersion + "/" + model.PromptTemplateVersion, SHA256: policy.BundleDigest(model.PromptTemplateVersion)}
	for _, kind := range []string{"kb", "claims"} {
		id, err := st.GetPointer(ctx, kind)
		if err != nil {
			return rec, fmt.Errorf("%s pointer: %w", kind, err)
		}
		r, err := st.GetRelease(ctx, id)
		if err != nil {
			return rec, err
		}
		name := map[string]string{"kb": "public_knowledge", "claims": "claim_registry"}[kind]
		rec.Artifacts[name] = Artifact{Version: id, SHA256: signing.Digest(r.Manifest)}
	}
	if in.EvalReportPath != "" {
		raw, err := os.ReadFile(in.EvalReportPath)
		if err != nil {
			return rec, err
		}
		// Only the summary fields are read (avoids importing eval, which
		// depends on this package through the orchestrator).
		var rep struct {
			DatasetVersion string `json:"dataset_version"`
			Verdict        string `json:"verdict"`
			Passed         int    `json:"passed"`
			Total          int    `json:"total"`
			P0Failed       int    `json:"p0_failed"`
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			return rec, fmt.Errorf("eval report: %w", err)
		}
		rec.AssuranceRuns["ai_eval"] = fmt.Sprintf("%s %s %d/%d passed, %d P0 failed (report sha256 %s)",
			rep.DatasetVersion, rep.Verdict, rep.Passed, rep.Total, rep.P0Failed, signing.Digest(raw))
	}
	return rec, nil
}
