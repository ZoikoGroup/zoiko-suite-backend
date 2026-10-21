package imports

import "zoiko.io/eventing/integration"

// OutcomeOf is a batch's integration outcome in the shared six-dimension model. It exists so that "the import
// succeeded" can never mean less than it says: a batch is CERTIFIED only when delivery, validation, mapping, the owning
// domain and the reconciliation each succeeded. A batch that was delivered but not yet reconciled is PENDING, one with
// exceptions or a reconciliation break is FAILED, and neither can be read as a success (INT-CTRL-036, INT-NP-048).
func OutcomeOf(b *Batch) integration.Outcome {
	o := integration.Outcome{
		Transport: integration.StatePending, Schema: integration.StatePending, Mapping: integration.StatePending,
		Provider: integration.StateNotApplicable, Domain: integration.StatePending, Reconciliation: integration.StatePending,
	}
	switch b.Status {
	case StatusCancelled:
		o.Transport, o.Schema, o.Mapping, o.Domain, o.Reconciliation = integration.StateNotApplicable, integration.StateNotApplicable,
			integration.StateNotApplicable, integration.StateNotApplicable, integration.StateNotApplicable
		return o
	case StatusQuarantined:
		o.Schema = integration.StateFailed // refused before anything was parsed
		return o
	case StatusFailed:
		o.Domain = integration.StateFailed
	}
	if b.ScanVerdict != "" && b.ScanVerdict != "CLEAN" && b.Status != StatusQuarantined {
		o.Schema = integration.StateFailed
	}

	validated := false
	switch b.Status {
	case StatusValidated, StatusPreviewed, StatusApproved, StatusImporting, StatusCompleted,
		StatusCompletedWithExceptions, StatusReconciled:
		validated = true
		if b.InvalidRows > 0 {
			o.Schema = integration.StateFailed
		} else {
			o.Schema = integration.StateSucceeded
		}
	}
	if validated && b.Status != StatusValidated && b.MappingHash != "" {
		o.Mapping = integration.StateSucceeded
	}

	switch b.Status {
	case StatusImporting:
		o.Transport, o.Domain = integration.StatePending, integration.StatePending
	case StatusCompleted, StatusCompletedWithExceptions, StatusReconciled:
		o.Transport, o.Domain = integration.StateSucceeded, integration.StateSucceeded
		if b.FailedItems > 0 {
			o.Transport = integration.StateFailed
		}
		if b.Status == StatusCompletedWithExceptions || b.QuarantinedItems > 0 || b.FailedItems > 0 {
			o.Domain = integration.StateFailed
		}
	}

	switch {
	case b.Reconciliation != nil && !b.Reconciliation.Matched:
		o.Reconciliation = integration.StateFailed
	case b.Status == StatusReconciled:
		o.Reconciliation = integration.StateSucceeded
	}
	return o
}
