package main

import (
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
)

// Doc 08 OPR-012: shutdown always outlasts a turn's lease (2×model timeout
// + 5 s, httpapi), so an in-flight turn finishes with a stored outcome.
func TestShutdownOutlastsTurnLease(t *testing.T) {
	for _, mt := range []time.Duration{6 * time.Second, 15 * time.Second, 30 * time.Second} {
		lease := 2*mt + 5*time.Second
		if got := shutdownTimeout(config.Config{ModelTimeout: mt}); got <= lease {
			t.Errorf("model timeout %s: shutdown %s does not outlast the %s lease", mt, got, lease)
		}
	}
}
