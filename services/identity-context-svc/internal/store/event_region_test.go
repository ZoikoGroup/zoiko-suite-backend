package store_test

import "zoiko.io/identity-context-svc/internal/outbox"

// Events are refused without a residency region (ZS-EVENT-001 section 14), so
// every test in this package that writes an event runs as a "uk" deployment.
func init() { outbox.SetEventRegion("uk") }
