package webhook

import "time"

// SetNowForTest pins the verifier clock so timestamp tests are deterministic.
func SetNowForTest(v *Verifier, now func() time.Time) { v.now = now }
