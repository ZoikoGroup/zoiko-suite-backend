package release

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
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
	ReleaseID   string              `json:"release_id"`
	Scope       string              `json:"scope"`
	CreatedAt   string              `json:"created_at"`
	Environment string              `json:"environment"`
	Artifacts   map[string]Artifact `json:"artifacts"`
	// PreviousReleases are the knowledge/claim release ids this one
	// replaces (the rollback targets), or NOT_RECORDED.
	PreviousReleases map[string]string `json:"previous_releases"`
	// RevocationWatermark is the highest revocation ledger entry at build
	// time: a rollback never goes below it (Doc 08 RB-02).
	RevocationWatermark int64             `json:"revocation_watermark"`
	ModelProfile        string            `json:"model_profile"`
	SchemaContract      string            `json:"schema_contract"`
	RolloutStage        string            `json:"rollout_stage"`
	AssuranceRuns       map[string]string `json:"assurance_runs"`
	Approvals           map[string]string `json:"approvals"`
	Decision            string            `json:"decision"`
	Limitations         []string          `json:"limitations"`
}

// ManifestInput is what the operator supplies.
type ManifestInput struct {
	ReleaseID   string
	GitCommit   string
	Environment string
	// ImageDigest is the immutable image reference, name@sha256:<64 hex>.
	ImageDigest  string
	RolloutStage string
	// ModelProfile is "<provider>:<model id>" as configured
	// (config.ModelProfile); the eval report must have been run with it.
	ModelProfile   string
	EvalReportPath string
	PolicyVersion  string
	// Evidence files; each is recorded with its sha256, or NOT_RECORDED.
	DrillReportPath   string
	SBOMPath          string
	ScanReportPath    string
	A11yReportPath    string
	FunctionalRunPath string
	// PreviousKB / PreviousClaims are the release ids this one replaces.
	PreviousKB     string
	PreviousClaims string
}

// NotRecorded marks evidence that was not supplied.
const NotRecorded = "NOT_RECORDED"

var imageRef = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)

// BuildManifest reads the currently promoted kb/claims releases, the
// revocation ledger and the evidence files, and returns the release record.
func BuildManifest(ctx context.Context, st store.Store, in ManifestInput) (ReleaseRecord, error) {
	rec := ReleaseRecord{
		ReleaseID: in.ReleaseID, Scope: "R0_PUBLIC_GUIDANCE", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Environment: in.Environment,
		Artifacts:   map[string]Artifact{}, PreviousReleases: map[string]string{"public_knowledge": NotRecorded, "claim_registry": NotRecorded},
		ModelProfile: in.ModelProfile, SchemaContract: "suivo-api-v1",
		RolloutStage: in.RolloutStage,
		AssuranceRuns: map[string]string{
			"functional": NotRecorded, "ai_eval": "NOT_EXECUTED", "security": NotRecorded,
			"accessibility": NotRecorded, "rollback_drill": NotRecorded, "sbom": NotRecorded,
		},
		Approvals: map[string]string{"product": "PENDING", "cto": "PENDING", "security": "PENDING", "dpo": "PENDING", "qa": "PENDING", "sre": "PENDING", "founder": "PENDING"},
		Decision:  "NO_GO_UNTIL_SIGNED",
		Limitations: []string{
			"All documents PROPOSED; no gate approved.",
			"Knowledge and claims are whatever releases are promoted; fixtures are TEST-ONLY.",
			"Numeric limits, TTLs and SLOs are unapproved dev defaults (DEC-07, DEC-08, D06-05).",
		},
	}
	if in.GitCommit == "" || in.ReleaseID == "" {
		return rec, fmt.Errorf("release id and git commit are required")
	}
	if in.Environment == "" {
		return rec, fmt.Errorf("environment is required")
	}
	if !imageRef.MatchString(in.ImageDigest) {
		return rec, fmt.Errorf("image must be an immutable reference name@sha256:<64 hex>, got %q", in.ImageDigest)
	}
	provider, _, ok := strings.Cut(in.ModelProfile, ":")
	if !ok || provider == "" {
		return rec, fmt.Errorf("model profile must be <provider>:<model id>, got %q", in.ModelProfile)
	}
	if provider == "mock" {
		rec.Limitations = append(rec.Limitations, "Model profile is the deterministic mock provider: not a production model (DEC-05).")
	} else {
		rec.Limitations = append(rec.Limitations, fmt.Sprintf("Model provider %s: DPA/region approval is a deployment input (PUBLIC_AI_PROVIDER_APPROVAL_REF), not verified here (DEC-05).", provider))
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
		prev := map[string]string{"kb": in.PreviousKB, "claims": in.PreviousClaims}[kind]
		if prev != "" {
			if err := checkPrevious(ctx, st, kind, id, prev); err != nil {
				return rec, err
			}
			rec.PreviousReleases[name] = prev
		}
	}
	revs, err := st.Revocations(ctx, 0)
	if err != nil {
		return rec, fmt.Errorf("revocation ledger: %w", err)
	}
	for _, r := range revs {
		if r.Watermark > rec.RevocationWatermark {
			rec.RevocationWatermark = r.Watermark
		}
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
			ModelProfile   string `json:"model_profile"`
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			return rec, fmt.Errorf("eval report: %w", err)
		}
		// Doc 08 §06: the evidence must be for the model that ships.
		if rep.ModelProfile != in.ModelProfile {
			return rec, fmt.Errorf("eval report model profile %q differs from the configured %q: re-run the evaluation", rep.ModelProfile, in.ModelProfile)
		}
		rec.AssuranceRuns["ai_eval"] = fmt.Sprintf("%s %s %d/%d passed, %d P0 failed, model %s (report sha256 %s)",
			rep.DatasetVersion, rep.Verdict, rep.Passed, rep.Total, rep.P0Failed, rep.ModelProfile, signing.Digest(raw))
	}
	if in.DrillReportPath != "" {
		raw, err := os.ReadFile(in.DrillReportPath)
		if err != nil {
			return rec, fmt.Errorf("drill report: %w", err)
		}
		var d struct {
			Passed int    `json:"passed"`
			Total  int    `json:"total"`
			Store  string `json:"store"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return rec, fmt.Errorf("drill report: %w", err)
		}
		rec.AssuranceRuns["rollback_drill"] = fmt.Sprintf("%d/%d drills passed (%s store) (report sha256 %s)", d.Passed, d.Total, d.Store, signing.Digest(raw))
	}
	for key, path := range map[string]string{"sbom": in.SBOMPath, "security": in.ScanReportPath, "accessibility": in.A11yReportPath, "functional": in.FunctionalRunPath} {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return rec, fmt.Errorf("%s evidence: %w", key, err)
		}
		rec.AssuranceRuns[key] = "evidence sha256 " + signing.Digest(raw)
	}
	return rec, nil
}

// checkPrevious verifies a stated previous release: it exists, is of the
// same kind, is not the active one, and, when the control audit records the
// last promotion for kind, is what that promotion replaced.
func checkPrevious(ctx context.Context, st store.Store, kind, active, prev string) error {
	r, err := st.GetRelease(ctx, prev)
	if err != nil {
		return fmt.Errorf("previous %s release %q: %w", kind, prev, err)
	}
	if r.Kind != kind {
		return fmt.Errorf("previous release %q is a %s release, not %s", prev, r.Kind, kind)
	}
	if prev == active {
		return fmt.Errorf("previous %s release %q is the active one", kind, prev)
	}
	audit, err := st.ListAudit(ctx, 1000)
	if err != nil {
		return err
	}
	for _, e := range audit { // newest first
		if e.Action == AuditPromote && e.Target == kind && e.AfterHash == store.Hash([]byte(active)) {
			if e.BeforeHash != store.Hash([]byte(prev)) {
				return fmt.Errorf("previous %s release %q is not what the last promotion of %q replaced (control audit)", kind, prev, active)
			}
			break
		}
	}
	return nil
}
