package whout

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/events"
)

type replayStub struct {
	Store
	replayed bool
	sub      *Subscription
}

func (r *replayStub) GetDelivery(context.Context, string, string) (*Delivery, error) {
	return &Delivery{DeliveryID: "d-1", SubscriptionID: "s-1", Status: DeliveryDeadLettered, DeadLetterReason: "5xx"}, nil
}
func (r *replayStub) GetSubscription(context.Context, string, string) (*Subscription, error) {
	return r.sub, nil
}
func (r *replayStub) Replay(context.Context, string, string, LogEntry, *events.Outbound, time.Time) (*Delivery, error) {
	r.replayed = true
	return &Delivery{DeliveryID: "d-1"}, nil
}

// INT-NP-045: a replay is refused when the subscription's destination region is not served by this deployment, so a
// replay cannot move data across a residency boundary.
func TestReplayRefusesADestinationRegionThisDeploymentDoesNotServe_INT_NP_045(t *testing.T) {
	st := &replayStub{sub: &Subscription{SubscriptionID: "s-1", Status: StatusActive, DestinationRegion: "us"}}
	svc, err := NewService(ServiceConfig{Store: st, Keys: nopKeys{}, AllowedRegions: []string{"uk"}})
	if err != nil {
		t.Fatal(err)
	}
	ap := ReplayApproval{Approver: "bob", ApprovalRef: "CHG-1", Reason: "recipient fixed"}
	_, err = svc.Replay(context.Background(), Actor{TenantID: "t", PrincipalID: "alice"}, "d-1", ap)
	if !errors.Is(err, ErrEgressDenied) || st.replayed {
		t.Fatalf("a replay into a foreign region must be refused before anything is queued: err=%v replayed=%v", err, st.replayed)
	}
	// negative control: the same replay in an allowed region is not refused by the region gate
	st.sub.DestinationRegion = "uk"
	if _, err = svc.Replay(context.Background(), Actor{TenantID: "t", PrincipalID: "alice"}, "d-1", ap); errors.Is(err, ErrEgressDenied) {
		t.Fatalf("an allowed region is not refused: %v", err)
	}
}

type nopKeys struct{}

func (nopKeys) Verify(context.Context, string, VaultCaller) error { return nil }
