package store

import "time"

// SetDriftConvergenceWindowForTest shortens the sweep's convergence window and
// returns a restore func. Test-only: this file is compiled into the test
// binary alone.
func SetDriftConvergenceWindowForTest(d time.Duration) (restore func()) {
	prev := driftConvergenceWindow
	driftConvergenceWindow = d
	return func() { driftConvergenceWindow = prev }
}
