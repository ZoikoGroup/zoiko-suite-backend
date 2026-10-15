package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNotificationJSON_NeverExposesGenericSent(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		n    Notification
		want string
	}{
		{Notification{Channel: ChannelEmail, Status: StatusSent, SentAt: &now}, DeliveryProviderAccepted},
		{Notification{Channel: ChannelInApp, Status: StatusSent, SentAt: &now}, DeliveryToInbox},
		{Notification{Channel: ChannelEmail, Status: StatusPendingUnknown}, DeliveryUnknown},
		{Notification{Channel: ChannelEmail, Status: StatusPending, NextAttemptAt: &now}, DeliveryRetryScheduled},
	} {
		raw, err := json.Marshal(c.n)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"status":"SENT"`) || !strings.Contains(string(raw), `"status":"`+c.want+`"`) {
			t.Fatalf("§3.3: %s", raw)
		}
		var back Notification
		if err := json.Unmarshal(raw, &back); err != nil || back.Status != c.n.Status {
			t.Fatalf("round trip lost the stored status: %v %q", err, back.Status)
		}
	}
}
