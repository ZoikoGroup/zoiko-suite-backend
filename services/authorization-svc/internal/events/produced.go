package events

// ProducedEventTypes is every event type authorization-svc writes to its
// topic (zoiko.authorization.events), all through the transactional outbox:
// the decision events with the decision row (DecisionEvents), the IAM, rule,
// session and exception events from the triggers of migrations 000026–000041,
// and the workload-binding and cache events from their handlers' transactions.
//
// It exists so asyncapi.yaml can be checked against the code in both
// directions (contract_test.go): an event emitted from Go or a migration
// that is not here fails, and an entry here that asyncapi.yaml does not
// declare fails.
var ProducedEventTypes = map[string]bool{
	// Decisions (ZS-IAM-001 §18; DecisionEvents).
	"authorization.granted":  true,
	"authorization.denied":   true,
	"sod.violation.detected": true,
	// GOV-03 InvalidateAuthorizationCache.
	"authorization.cache.invalidated": true,
	// §23 IAM events (000026, 000037/000038/000041).
	"iam.role.published":          true,
	"iam.assignment.granted":      true,
	"iam.assignment.revoked":      true,
	"iam.assignment.suspended":    true,
	"iam.assignment.resumed":      true,
	"iam.assignment.expired":      true,
	"iam.delegation.granted":      true,
	"iam.delegation.revoked":      true,
	"iam.authority_limit.changed": true,
	"iam.sod_policy.published":    true,
	"iam.policy_set.published":    true,
	// AU-17 governed rule changes (000036).
	"iam.sod_rule.change_requested":  true,
	"iam.sod_rule.change_rejected":   true,
	"iam.abac_rule.change_requested": true,
	"iam.abac_rule.change_rejected":  true,
	// Workload bindings (N-09).
	"iam.workload_binding.saved":       true,
	"iam.workload_binding.deactivated": true,
	"iam.workload_binding.reactivated": true,
	// Sessions (000030, 000034).
	"security.privileged_session.started":  true,
	"security.privileged_session.ended":    true,
	"security.break_glass.requested":       true,
	"security.break_glass.approval_recorded": true,
	"security.break_glass.started":         true,
	"security.break_glass.ended":           true,
	"security.break_glass.rejected":        true,
	"security.break_glass.expired":         true,
	"support.session.started":              true,
	"support.session.ended":                true,
	// GOV-04 compensating-control exceptions (000027).
	"sod.compensating_control.approved": true,
	"sod.compensating_control.revoked":  true,
	"sod.exception.expired":             true,
}

// AllConsumedEventTypes is every event type either consumer acts on.
func AllConsumedEventTypes() map[string]bool {
	out := map[string]bool{}
	for k := range ConsumedEventTypes {
		out[k] = true
	}
	for k := range LifecycleConsumedEventTypes() {
		out[k] = true
	}
	return out
}
