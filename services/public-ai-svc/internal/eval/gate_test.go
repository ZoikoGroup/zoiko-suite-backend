package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/policy"
)

func labelled(id, intent string) Case {
	var c Case
	c.ID, c.Priority, c.Partition = id, "P1", "A"
	c.Assert.Intent = intent
	return c
}

// Doc 07 §10: macro intent accuracy below 0.97 is a P1 NO_GO, even when
// every case's other assertions pass.
func TestMacroIntentAccuracyGate(t *testing.T) {
	// answerFunc always returns I12: I12 cases are correct, I05 wrong.
	cases := []Case{labelled("a", "I12"), labelled("b", "I12"), labelled("c", "I05")}
	cases[2].Assert.Intent = "" // unlabelled: still graded, not in the metric
	rep := RunWith(context.Background(), answerFunc("ABSTAIN", "x"), Dataset{Cases: cases}, Options{})
	if rep.Verdict != VerdictPass || rep.Intents.Macro != 1 {
		t.Fatalf("all labelled intents correct: verdict %s macro %v gates %v", rep.Verdict, rep.Intents.Macro, rep.Intents.GateFailures)
	}

	cases[2].Assert.Intent = "I05"
	rep = RunWith(context.Background(), answerFunc("ABSTAIN", "x"), Dataset{Cases: cases}, Options{})
	if rep.Intents.Macro >= MinMacroIntentAccuracy || rep.Verdict != VerdictNoGo || len(rep.Intents.GateFailures) == 0 {
		t.Fatalf("macro %v must be NO_GO, got %s %v", rep.Intents.Macro, rep.Verdict, rep.Intents.GateFailures)
	}
	// A waiver on the failing case does not lift the intent gate.
	rep.ApplyWaivers(map[string]string{"c": "W-1"})
	if rep.Verdict != VerdictNoGo {
		t.Fatalf("intent gate must not be waived per case: %s", rep.Verdict)
	}
}

// Doc 07 §10: every launch intent needs labelled support.
func TestLaunchIntentsNeedSupport(t *testing.T) {
	d := Dataset{LaunchIntents: []string{"I12", "I24"}, Cases: []Case{labelled("a", "I12")}}
	rep := RunWith(context.Background(), answerFunc("ABSTAIN", "x"), d, Options{})
	if rep.Verdict != VerdictNoGo || len(rep.Intents.Unsupported) != 1 || rep.Intents.Unsupported[0] != "I24" {
		t.Fatalf("verdict %s unsupported %v", rep.Verdict, rep.Intents.Unsupported)
	}
}

// Doc 07 §06/§19 (REL-QA-02): the report records what was evaluated.
func TestReportProvenance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.json")
	raw := []byte(`{"dataset_version":"v","note":"n","cases":[]}`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rep := RunWith(context.Background(), answerFunc("ABSTAIN", "x"), d,
		Options{ModelProfile: "fake-v0", PolicyVersion: "pub-policy-1.0", ClaimsReleaseID: "claims-1"})
	if sum := sha256.Sum256(raw); rep.DatasetSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("dataset digest %q", rep.DatasetSHA256)
	}
	if rep.PromptVersion != model.PromptVersion || rep.PolicyDigest != policy.BundleDigest(model.PromptTemplateVersion) ||
		rep.PolicyVersion != "pub-policy-1.0" || rep.ClaimsReleaseID != "claims-1" || rep.ModelID != "fake-v0" {
		t.Fatalf("provenance %+v", rep)
	}
	if !strings.Contains(rep.PromptVersion, "@") {
		t.Fatal("prompt version must carry its content hash")
	}
}

// The approved baseline (evalrun -baseline) covers every golden case, so a
// new case cannot silently sit outside the regression check.
func TestBaselineCoversGoldenSet(t *testing.T) {
	dir := filepath.Join(fixtureenv.FixturesDir(), "..", "golden")
	d, err := Load(filepath.Join(dir, "golden-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var base Report
	if err := json.Unmarshal(b, &base); err != nil {
		t.Fatal(err)
	}
	passed := map[string]bool{}
	for _, r := range base.Results {
		passed[r.ID] = r.Pass
	}
	if base.DatasetVersion != d.DatasetVersion {
		t.Fatalf("baseline is for %s, dataset is %s: regenerate the baseline", base.DatasetVersion, d.DatasetVersion)
	}
	for _, c := range d.Cases {
		if !passed[c.ID] {
			t.Errorf("%s is not a passing case in baseline.json", c.ID)
		}
	}
}
