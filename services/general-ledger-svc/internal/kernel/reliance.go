package kernel

import (
	"context"
	"errors"

	"zoiko.io/general-ledger-svc/internal/certreliance"
)

// CertificationReliance records that a posting relied on a certification (internal/certreliance). It is optional on the
// Service: a deployment that has not wired it refuses to post an event that cites a certification, and an event that cites none
// never reaches it.
type CertificationReliance interface {
	RecordReliance(ctx context.Context, tenantID, principalID, correlationID string, ref certreliance.Ref, obj certreliance.Object) error
}

// relianceObjectID is the identity a kernel posting is recorded under: the accounting event, the book and the posting basis,
// which is exactly what makes a posting idempotent. It is stable across retries (unlike the journal id, which is new on every
// attempt), so a posting that fails and is retried records ONE reliance, not one per attempt.
func relianceObjectID(eventID, bookID, basis string) string {
	return eventID + ":" + bookID + ":" + basis
}

// recordReliance runs just before the posting transaction. For each certification the accounting event's evidence cites
// ("certification:<service>:<run id>") it records the posting's reliance with the service that issued it, and it fails CLOSED:
//
//   - a certification that is revoked, superseded or past its validity window refuses the posting (AK-RULE/CERTIFICATION_NOT_IN_FORCE,
//     recorded as a FAILED execution): nothing is posted on withdrawn evidence;
//   - a malformed reference, or one naming a certification that does not exist, cannot be fixed by retrying, so the execution is
//     QUARANTINED for a person to decide (AK-DATA);
//   - a principal not permitted to record reliance is AK-AUTH, and like every other authorization refusal it records no execution;
//   - a certification service that cannot answer, or is not wired, is AK-EXT (retryable, 503), exactly as an unavailable period
//     service is.
//
// Recording is idempotent and happens before the journal is written, so a posting that fails afterwards leaves a reliance for a
// posting that did not happen: that over-names it in a blast radius, never under-names it.
func (s *Service) recordReliance(ctx context.Context, tenant string, a Actor, ev *AccountingEvent, bookID, basis string) error {
	refs, err := certreliance.ParseRefs(ev.EvidenceRefs)
	if err != nil {
		return exc(FamData, "CERTIFICATION_REF_INVALID", "%v", err)
	}
	if len(refs) == 0 {
		return nil
	}
	if s.Reliance == nil {
		return exc(FamExt, "CERTIFICATION_SERVICE_UNAVAILABLE", "the event cites a certification but reliance recording is not configured")
	}
	obj := certreliance.Object{Type: certreliance.ObjectTypeAccountingPosting, ID: relianceObjectID(ev.AccountingEventID, bookID, basis), LegalEntityID: ev.LegalEntityID}
	for _, ref := range refs {
		err := s.Reliance.RecordReliance(ctx, tenant, a.PrincipalID, a.CorrelationID, ref, obj)
		switch {
		case err == nil:
		case errors.Is(err, certreliance.ErrNotInForce):
			return exc(FamRule, "CERTIFICATION_NOT_IN_FORCE", "%s is revoked, superseded or past its validity window", ref).with("certification", ref.String())
		case errors.Is(err, certreliance.ErrUnknown):
			return exc(FamData, "CERTIFICATION_UNKNOWN", "%s does not exist", ref).with("certification", ref.String())
		case errors.Is(err, certreliance.ErrDenied):
			return exc(FamAuth, "CERTIFICATION_RELIANCE_DENIED", "not permitted to record reliance on %s", ref).with("certification", ref.String())
		default:
			return exc(FamExt, "CERTIFICATION_SERVICE_UNAVAILABLE", "reliance on %s could not be recorded: %v", ref, err).with("certification", ref.String())
		}
	}
	return nil
}

// validateEvidenceRefs is the registration-time half: an event that cites a certification in a form the posting path would
// refuse can never post, so it is refused when it is registered rather than discovered when someone tries to post it.
func validateEvidenceRefs(refs []string) error {
	_, err := certreliance.ParseRefs(refs)
	return err
}
