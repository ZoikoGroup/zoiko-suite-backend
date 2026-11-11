package inbox

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"sync"
	"testing"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/policy"
)

func ukMsg(id, region, class string) Message {
	m := msg(id)
	m.Region, m.Classification = region, class
	return m
}

// EV-T10 at the consumer: a restricted UK event reaching a US consumer is
// denied before any write, and the denial is recorded as evidence.
func TestResidencyDenialRecordsEvidence_EV_T10(t *testing.T) {
	in, _ := New(PoolRunner{}, "us-consumer", DefaultConfig()) // no database needed: denial happens first
	pol, _ := policy.NewResidency(nil)
	var got []policy.Decision
	in.SetDeliveryPolicy(pol, "us", func(d policy.Decision) { got = append(got, d) })

	res, err := in.Process(context.Background(), ukMsg("e1", "uk", "restricted"), handler)
	if !errors.Is(err, ErrDeliveryDenied) || res.Outcome != Rejected {
		t.Fatalf("want ErrDeliveryDenied/Rejected, got %v / %v", err, res.Outcome)
	}
	if len(got) != 1 || got[0].EventRegion != "uk" || got[0].ConsumerRegion != "us" || got[0].Consumer != "us-consumer" || got[0].Allowed {
		t.Fatalf("control evidence not recorded: %+v", got)
	}
}

// A message that names no classification is treated as restricted.
func TestMissingClassificationTreatedAsRestricted(t *testing.T) {
	in, _ := New(PoolRunner{}, "c", DefaultConfig())
	pol, _ := policy.NewResidency([]policy.TransferRule{{From: "uk", To: "us", MaxClassification: envelope.Confidential, Purpose: "p"}})
	in.SetDeliveryPolicy(pol, "us", nil)
	if _, err := in.Process(context.Background(), ukMsg("e2", "uk", ""), handler); !errors.Is(err, ErrDeliveryDenied) {
		t.Fatalf("unclassified event must be denied across regions: %v", err)
	}
}

// Same-region delivery is processed normally and the policy is invisible.
func TestSameRegionDeliveryProcessed(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	pol, _ := policy.NewResidency(nil)
	in.SetDeliveryPolicy(pol, "uk", nil)
	if r, err := in.Process(context.Background(), ukMsg("e3", "uk", "restricted"), handler); err != nil || r.Outcome != Processed {
		t.Fatalf("%v %v", r.Outcome, err)
	}
}

func TestParseReadsClassification(t *testing.T) {
	m, err := Parse([]byte(`{"id":"1","type":"t","tenantid":"x","payloadhash":"sha256:a","residencyregion":"uk","classification":"restricted"}`))
	if err != nil || m.Classification != "restricted" || m.Region != "uk" {
		t.Fatalf("%+v %v", m, err)
	}
}

// Observability (section 17): denied, rejected-replay, processed, duplicate and
// quarantined deliveries are all visible to a metrics hook.
func TestObserverSeesEveryOutcome(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	var mu sync.Mutex
	var obs []Observation
	in.SetObserver(func(o Observation) { mu.Lock(); obs = append(obs, o); mu.Unlock() })
	ctx := context.Background()

	in.Process(ctx, msg("o1"), handler) // processed
	in.Process(ctx, msg("o1"), handler) // duplicate
	rm := msg("o2")
	rm.ReplayRunID = "run-1"
	in.Process(ctx, rm, handler)                                                                                                // replay rejected
	in.Process(ctx, msg("o3"), func(context.Context, pgx.Tx, Receipt) (string, error) { return "", Poison(errors.New("bad")) }) // quarantined

	mu.Lock()
	defer mu.Unlock()
	if len(obs) != 4 {
		t.Fatalf("want 4 observations, got %d", len(obs))
	}
	if obs[0].Outcome != Processed || obs[1].Outcome != Duplicate {
		t.Errorf("processed/duplicate not observed: %+v %+v", obs[0], obs[1])
	}
	if obs[2].Outcome != Rejected || !obs[2].Replay || !errors.Is(obs[2].Err, ErrReplayRejected) {
		t.Errorf("rejected replay not observed: %+v", obs[2])
	}
	if !obs[3].Quarantined || obs[3].Err == nil {
		t.Errorf("quarantine not observed: %+v", obs[3])
	}
	if obs[0].Consumer != "test-consumer" || obs[0].EventType == "" {
		t.Errorf("observation lacks identity: %+v", obs[0])
	}
}

func TestObserverSeesDenial(t *testing.T) {
	in, _ := New(PoolRunner{}, "c", DefaultConfig())
	pol, _ := policy.NewResidency(nil)
	in.SetDeliveryPolicy(pol, "us", nil)
	var o Observation
	in.SetObserver(func(x Observation) { o = x })
	in.Process(context.Background(), ukMsg("d1", "uk", "public"), handler)
	if !o.Denied || o.Outcome != Rejected {
		t.Fatalf("denial not observed: %+v", o)
	}
}
