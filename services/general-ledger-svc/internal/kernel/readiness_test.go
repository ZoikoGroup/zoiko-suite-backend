package kernel

import (
	"context"
	"testing"
	"time"
)

type readyRef struct {
	maps   map[string]string
	active map[string]bool
	header map[string]bool
}

func (r readyRef) AccountFacts(_ context.Context, _, code string) (bool, bool, bool, error) {
	a, ok := r.active[code]
	return a, !r.header[code], ok, nil
}
func (r readyRef) MappedAccount(_ context.Context, _, key string, _ time.Time) (string, bool, error) {
	c, ok := r.maps[key]
	return c, ok, nil
}
func (r readyRef) DimensionAllowed(context.Context, string, string, string, string, time.Time) (bool, error) {
	return true, nil
}

type readyRepo struct {
	Repo
	dims []Dimension
	vals map[string][]DimensionValue
}

func (r readyRepo) ListDimensions(context.Context, string) ([]Dimension, error) { return r.dims, nil }
func (r readyRepo) ListDimensionValues(_ context.Context, _, d string) ([]DimensionValue, error) {
	return r.vals[d], nil
}

func readinessItem(t *testing.T, rep *ReadinessReport, kind, key string) ReadinessItem {
	t.Helper()
	for _, it := range rep.Items {
		if it.Kind == kind && it.Key == key {
			return it
		}
	}
	t.Fatalf("no %s item %q in %+v", kind, key, rep.Items)
	return ReadinessItem{}
}

func TestReadinessFindsEveryGapAPackNeeds(t *testing.T) {
	pack, _ := PackByName("ar-standard")
	on := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	rules := pack.Drafts("1", on, BookStatutory, "IFRS")

	// nothing configured: every mapping key is unmapped and the pack is not ready
	rep, err := CheckReadiness(context.Background(), "t", "pack:ar-standard", rules, readyRef{}, readyRepo{}, on)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ready || rep.Blocking == 0 || rep.RuleCount != len(rules) {
		t.Fatalf("an unconfigured tenant must not be ready: %+v", rep)
	}
	it := readinessItem(t, rep, "MAPPING", "AR_CONTROL")
	if it.State != ReadyUnmapped || !it.Blocking || it.Remedy == "" || len(it.UsedBy) == 0 {
		t.Fatalf("AR_CONTROL: %+v", it)
	}

	// map all keys to good accounts: ready
	ref := readyRef{maps: map[string]string{}, active: map[string]bool{}, header: map[string]bool{}}
	for _, it := range rep.Items {
		if it.Kind == "MAPPING" {
			ref.maps[it.Key] = "A" + it.Key
			ref.active["A"+it.Key] = true
		}
	}
	rep, err = CheckReadiness(context.Background(), "t", "pack", rules, ref, readyRepo{}, on)
	if err != nil || !rep.Ready || rep.Blocking != 0 {
		t.Fatalf("fully mapped must be ready: %v %+v", err, rep)
	}

	// each way a mapped account can still be unusable
	ref.header["AAR_CONTROL"] = true
	ref.active["AREVENUE"] = false
	delete(ref.active, "AOUTPUT_TAX")
	rep, _ = CheckReadiness(context.Background(), "t", "pack", rules, ref, readyRepo{}, on)
	for key, want := range map[string]string{"AR_CONTROL": ReadyAccountHeader, "REVENUE": ReadyAccountInactive, "OUTPUT_TAX": ReadyAccountMissing} {
		if got := readinessItem(t, rep, "MAPPING", key); got.State != want || !got.Blocking {
			t.Fatalf("%s: want %s got %+v", key, want, got)
		}
	}
	if rep.Ready {
		t.Fatal("unusable accounts must block")
	}
}

func TestReadinessChecksDimensionsAndTheirValues(t *testing.T) {
	on := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	rules := []RuleSet{{EventType: "X.Y", Content: RuleContent{
		LineTemplates: []LineTemplate{{LineRef: "L1", Side: Debit, Account: AccountRef{AccountCode: "1000"}, Amount: "amount"}},
		DimensionRules: []DimensionRule{
			{Dimension: "cost_center", Mode: DimRequired},
			{Dimension: "project", Mode: DimOptional},
			{Dimension: "region", Mode: DimRequired},
			{Dimension: "secret", Mode: DimProhibited},
		},
	}}}
	valid := DimensionValue{ValueCode: "CC1", Status: "ACTIVE", ValidFrom: on.AddDate(0, -1, 0)}
	future := DimensionValue{ValueCode: "CC2", Status: "ACTIVE", ValidFrom: on.AddDate(0, 1, 0)}
	repo := readyRepo{
		dims: []Dimension{{DimensionCode: "cost_center", Status: "ACTIVE"}, {DimensionCode: "region", Status: "ACTIVE"}, {DimensionCode: "project", Status: "RETIRED"}},
		vals: map[string][]DimensionValue{"cost_center": {valid}, "region": {future}},
	}
	rep, err := CheckReadiness(context.Background(), "t", "x", rules, readyRef{}, repo, on)
	if err != nil {
		t.Fatal(err)
	}
	if got := readinessItem(t, rep, "DIMENSION", "cost_center"); got.State != ReadyOK {
		t.Fatalf("cost_center: %+v", got)
	}
	if got := readinessItem(t, rep, "DIMENSION", "region"); got.State != ReadyDimNoValues || !got.Blocking {
		t.Fatalf("a value valid only in the future is no value today: %+v", got)
	}
	if got := readinessItem(t, rep, "DIMENSION", "project"); got.State != ReadyDimRetired {
		t.Fatalf("project: %+v", got)
	}
	for _, it := range rep.Items {
		if it.Key == "secret" {
			t.Fatal("a PROHIBITED dimension is not a need")
		}
	}

	rep, _ = CheckReadiness(context.Background(), "t", "x", rules, readyRef{}, readyRepo{}, on)
	if got := readinessItem(t, rep, "DIMENSION", "cost_center"); got.State != ReadyDimMissing {
		t.Fatalf("unregistered dimension: %+v", got)
	}
}
