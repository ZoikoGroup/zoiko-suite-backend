package eval

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/fixtureenv"
)

// The golden set runs in CI: any P0 failure fails the build (Doc 07 §19).
func TestGoldenSet(t *testing.T) {
	ctx := context.Background()
	env, err := fixtureenv.New(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Load(filepath.Join(fixtureenv.FixturesDir(), "..", "golden", "golden-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep := Run(ctx, env.Pipeline, d, "fake-v0")
	for _, r := range rep.Results {
		if !r.Pass {
			t.Errorf("%s (%s/%s): %v", r.ID, r.Partition, r.Priority, r.Failures)
		}
	}
	if rep.Verdict != "PASS" || rep.Total != len(d.Cases) || rep.Total < 20 {
		t.Fatalf("verdict %s, %d/%d passed", rep.Verdict, rep.Passed, rep.Total)
	}
}

// The grader itself must catch a bad answer (a test of the test).
func TestGraderCatchesFabrication(t *testing.T) {
	var c Case
	c.ID, c.Priority = "x", "P0"
	c.Assert.Outcome = "ABSTAIN"
	c.Assert.MustNotContain = []string{"certified"}
	bad := Run(context.Background(), answerFunc("ANSWERED", "We are SOC 2 certified."), Dataset{Cases: []Case{c}}, "test")
	if bad.Verdict != "BLOCK_RELEASE" || bad.P0Failed != 1 {
		t.Fatalf("grader missed a fabricated certification: %+v", bad)
	}
}
