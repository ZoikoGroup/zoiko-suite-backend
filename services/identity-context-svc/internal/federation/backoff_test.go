package federation

import (
	"context"
	"testing"
	"time"
)

type nilProvRepo struct{ ProvisionRepo }
type nilAC struct{ AccessControl }
type nilEvict struct{}

func (nilEvict) EvictAllForPrincipal(context.Context, string, string) (int, error) { return 0, nil }

func TestBackoff(t *testing.T) {
	p, err := NewProvisioning(nilProvRepo{}, newMemRepo(), nilAC{}, nilEvict{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	p.SetClock(func() time.Time { return now })
	p.BackoffBase, p.BackoffMax, p.RevokeBackoffMax = 10*time.Second, 15*time.Minute, time.Minute

	delay := func(status string, attempts int) time.Duration { return p.backoff(status, attempts).Sub(now) }
	if delay(AsgPlanned, 1) != 10*time.Second {
		t.Fatalf("the first retry waits the base interval: %v", delay(AsgPlanned, 1))
	}
	if delay(AsgPlanned, 2) != 20*time.Second || delay(AsgPlanned, 3) != 40*time.Second {
		t.Fatal("it doubles")
	}
	if delay(AsgPlanned, 50) != 15*time.Minute {
		t.Fatalf("grants back off to the ceiling: %v", delay(AsgPlanned, 50))
	}
	if delay(AsgRevokePending, 1) != 10*time.Second {
		t.Fatal("same start")
	}
	if delay(AsgRevokePending, 50) != time.Minute {
		t.Fatalf("taking access away is the priority path and never waits more than a minute: %v", delay(AsgRevokePending, 50))
	}
	// a configured ceiling below the revoke ceiling wins
	p.BackoffMax = 30 * time.Second
	if delay(AsgRevokePending, 50) != 30*time.Second {
		t.Fatalf("%v", delay(AsgRevokePending, 50))
	}
}

func TestNewProvisioningRefusesToBeBuiltWithoutWhatDeprovisioningNeeds(t *testing.T) {
	for name, args := range map[string][4]any{
		"no store":           {nil, newMemRepo(), nilAC{}, nilEvict{}},
		"no conns":           {nilProvRepo{}, nil, nilAC{}, nilEvict{}},
		"no access control":  {nilProvRepo{}, newMemRepo(), nil, nilEvict{}},
		"no session evictor": {nilProvRepo{}, newMemRepo(), nilAC{}, nil},
	} {
		var repo ProvisionRepo
		if args[0] != nil {
			repo = args[0].(ProvisionRepo)
		}
		var conns Repo
		if args[1] != nil {
			conns = args[1].(Repo)
		}
		var ac AccessControl
		if args[2] != nil {
			ac = args[2].(AccessControl)
		}
		var ev SessionEvictor
		if args[3] != nil {
			ev = args[3].(SessionEvictor)
		}
		if _, err := NewProvisioning(repo, conns, ac, ev, nil); err == nil {
			t.Errorf("%s: a deployment that cannot end sessions or revoke access must not accept deprovisioning", name)
		}
	}
}
