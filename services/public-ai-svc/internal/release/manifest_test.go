package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/signing"
)

const testImage = "ghcr.io/zoiko/public-ai-svc@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func manifestInput() ManifestInput {
	return ManifestInput{ReleaseID: "suivo-r0-test-01", GitCommit: "abc123", Environment: "staging", ImageDigest: testImage,
		RolloutStage: "staging", ModelProfile: "groq:openai/gpt-oss-120b", PolicyVersion: "pub-policy-1.0"}
}

// Doc 08 §06: the image is an immutable digest reference, never a tag.
func TestManifestRequiresImageDigest(t *testing.T) {
	e := setup(t)
	for _, img := range []string{"", "public-ai-svc:latest", "sha256:abc", "public-ai-svc@sha256:ABC", "public-ai-svc@sha256:" + strings.Repeat("a", 63)} {
		in := manifestInput()
		in.ImageDigest = img
		if _, err := BuildManifest(ctx, e.st, in); err == nil {
			t.Errorf("image %q must be refused", img)
		}
	}
	if _, err := BuildManifest(ctx, e.st, manifestInput()); err != nil {
		t.Fatal(err)
	}
}

// Doc 08 §06/§33: the eval evidence must be for the model that ships.
func TestManifestRefusesEvalForOtherModel(t *testing.T) {
	e := setup(t)
	in := manifestInput()
	in.EvalReportPath = writeFile(t, "eval.json", `{"dataset_version":"golden-v1","verdict":"PASS","passed":1,"total":1,"model_profile":"mock:mock-extractive-v1"}`)
	if _, err := BuildManifest(ctx, e.st, in); err == nil || !strings.Contains(err.Error(), "model profile") {
		t.Fatalf("eval for another model must be refused: %v", err)
	}
	in.ModelProfile = "mock:mock-extractive-v1"
	rec, err := BuildManifest(ctx, e.st, in)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ModelProfile != "mock:mock-extractive-v1" || !strings.Contains(rec.AssuranceRuns["ai_eval"], "PASS 1/1") {
		t.Fatalf("eval not recorded: %+v", rec.AssuranceRuns)
	}
}

// Evidence is recorded by sha256 or NOT_RECORDED, never by a hardcoded
// claim; environment, revocation watermark and previous releases are kept.
func TestManifestRecordsEvidenceDigests(t *testing.T) {
	e := setup(t)
	rec, err := BuildManifest(ctx, e.st, manifestInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"rollback_drill", "sbom", "security", "accessibility", "functional"} {
		if rec.AssuranceRuns[k] != NotRecorded {
			t.Errorf("%s = %q without evidence, want NOT_RECORDED", k, rec.AssuranceRuns[k])
		}
	}
	for _, l := range rec.Limitations {
		if strings.Contains(l, "fake provider") || strings.Contains(l, "widget not built") {
			t.Errorf("hardcoded limitation: %s", l)
		}
	}
	if _, err := Revoke(ctx, e.st, "claim", "c-x", "TEST", nil); err != nil {
		t.Fatal(err)
	}
	drill := `{"passed":7,"total":7,"store":"memory"}`
	sbom := `{"bomFormat":"CycloneDX"}`
	in := manifestInput()
	in.DrillReportPath, in.SBOMPath, in.ScanReportPath = writeFile(t, "drill.json", drill), writeFile(t, "sbom.json", sbom), writeFile(t, "scan.json", "{}")
	in.PreviousKB = "kb-test-1"
	if _, err := BuildManifest(ctx, e.st, in); err == nil {
		t.Fatal("the active release cannot be its own previous release")
	}
	in.PreviousKB = "claims-test-1"
	if _, err := BuildManifest(ctx, e.st, in); err == nil {
		t.Fatal("a claims release cannot be the previous kb release")
	}
	in.PreviousKB = ""
	rec, err = BuildManifest(ctx, e.st, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.AssuranceRuns["rollback_drill"], "7/7") || !strings.Contains(rec.AssuranceRuns["rollback_drill"], signing.Digest([]byte(drill))) {
		t.Errorf("drill evidence: %s", rec.AssuranceRuns["rollback_drill"])
	}
	if rec.AssuranceRuns["sbom"] != "evidence sha256 "+signing.Digest([]byte(sbom)) {
		t.Errorf("sbom evidence: %s", rec.AssuranceRuns["sbom"])
	}
	if rec.Environment != "staging" || rec.RevocationWatermark < 1 || rec.PreviousReleases["public_knowledge"] != NotRecorded {
		t.Errorf("environment/watermark/previous: %q %d %v", rec.Environment, rec.RevocationWatermark, rec.PreviousReleases)
	}
}
