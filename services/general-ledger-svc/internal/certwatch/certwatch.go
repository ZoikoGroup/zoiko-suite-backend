// Package certwatch is how the general ledger acts when a certification it relied on is withdrawn (ZS-SVC-Z-001 RB-05, DQC-017).
//
// Recording reliance (internal/certreliance) tells the issuing service what rests on a certification. This package closes the
// loop in the other direction:
//
//   - the Recorder WATCHES every certification the ledger relies on, at the moment it records the reliance;
//   - the Sweeper asks each issuing service whether a watched certification is still in force and, when one has been REVOKED,
//     FLAGS every posted journal and kernel posting that cites it;
//   - a person ACKNOWLEDGES each flag with a reason (and may name the journal that remediated it).
//
// It never reverses anything: whether a posted journal is reversed is a financial decision, so a flag is evidence for a person,
// and the acknowledgement is that person's recorded decision.
//
// It PULLS rather than consuming the withdrawal events, deliberately. An event can be lost, delayed or published in a different
// shape by each producer; the issuing service's own answer is authoritative, and a sweep needs no broker. The withdrawal events
// remain the way to be told sooner; a sweep after any outage catches what an event missed.
package certwatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"zoiko.io/general-ledger-svc/internal/certreliance"
)

var (
	ErrFlagNotFound            = errors.New("certification flag not found")
	ErrFlagAlreadyAcknowledged = errors.New("the certification flag is already acknowledged")
	ErrInvalid                 = errors.New("invalid certification watch request")
)

// Flag statuses.
const (
	FlagOpen         = "OPEN"
	FlagAcknowledged = "ACKNOWLEDGED"
)

// Watched is one certification the ledger relies on, for one legal entity.
type Watched struct {
	LegalEntityID string
	Ref           certreliance.Ref
	LastStatus    string
	WithdrawnAt   *time.Time
	RevocationID  string
	Reason        string
}

// Flag is one posted journal or posting found resting on a withdrawn certification.
type Flag struct {
	FlagID               string     `json:"flag_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	Certification        string     `json:"certification"`
	SubjectType          string     `json:"subject_type"`
	SubjectID            string     `json:"subject_id"`
	JournalID            string     `json:"journal_id"`
	RevocationID         string     `json:"revocation_id"`
	Reason               string     `json:"reason"`
	Status               string     `json:"status"`
	RaisedAt             time.Time  `json:"raised_at"`
	AcknowledgedBy       string     `json:"acknowledged_by,omitempty"`
	AcknowledgedAt       *time.Time `json:"acknowledged_at,omitempty"`
	Acknowledgement      string     `json:"acknowledgement,omitempty"`
	RemediationJournalID string     `json:"remediation_journal_id,omitempty"`
}

// Store is the persistence the watch needs; every method is tenant-scoped.
type Store interface {
	// Watch registers that the legal entity relies on the certification. Idempotent.
	Watch(ctx context.Context, tenant, legalEntityID string, ref certreliance.Ref) error
	ListWatched(ctx context.Context, tenant, legalEntityID string) ([]Watched, error)
	// ListWatchers is every legal entity of the tenant that relies on one certification (the entities a withdrawal event concerns).
	ListWatchers(ctx context.Context, tenant string, ref certreliance.Ref) ([]Watched, error)
	RecordCheck(ctx context.Context, tenant, legalEntityID string, ref certreliance.Ref, state string) error
	// RecordWithdrawal marks the watched certification withdrawn (once; an already withdrawn one keeps its first revocation) and
	// flags everything of that entity that cites it and is not flagged yet, returning how many flags are new. Idempotent, so it is
	// also how a journal posted after the withdrawal was first recorded is still caught.
	RecordWithdrawal(ctx context.Context, tenant, legalEntityID string, ref certreliance.Ref, revocationID, reason string) (newFlags int, err error)
	ListFlags(ctx context.Context, tenant, legalEntityID, status string) ([]Flag, error)
	GetFlag(ctx context.Context, tenant, flagID string) (*Flag, error)
	Acknowledge(ctx context.Context, tenant, flagID, by, acknowledgement string, remediationJournalID string) (*Flag, error)
}

// RelianceRecorder is the recording half (internal/certreliance.Client).
type RelianceRecorder interface {
	RecordReliance(ctx context.Context, tenantID, principalID, correlationID string, ref certreliance.Ref, obj certreliance.Object) error
}

// StatusReader is the checking half (internal/certreliance.Client).
type StatusReader interface {
	CertificationStatus(ctx context.Context, tenantID, principalID, correlationID string, ref certreliance.Ref) (certreliance.Status, error)
}

// Recorder records reliance with the issuing service and then WATCHES the certification, so the ledger can find out later that it
// was withdrawn. If the watch cannot be registered the whole recording fails (and with it the posting): posting on a certification
// nobody will ever check again is the silence this package exists to end.
type Recorder struct {
	Reliance RelianceRecorder
	Store    Store
}

func (r *Recorder) RecordReliance(ctx context.Context, tenantID, principalID, correlationID string, ref certreliance.Ref, obj certreliance.Object) error {
	if err := r.Reliance.RecordReliance(ctx, tenantID, principalID, correlationID, ref, obj); err != nil {
		return err
	}
	if err := r.Store.Watch(ctx, tenantID, obj.LegalEntityID, ref); err != nil {
		return fmt.Errorf("%w: the certification could not be put on the watch list: %v", certreliance.ErrUnavailable, err)
	}
	return nil
}

// Outcome is what a sweep found for one watched certification.
type Outcome struct {
	Certification string `json:"certification"`
	State         string `json:"state,omitempty"`
	Withdrawn     bool   `json:"withdrawn"`
	NewFlags      int    `json:"new_flags"`
	Error         string `json:"error,omitempty"`
}

// SweepResult is a sweep's account of itself. A sweep that could not check a certification says so; it never reports a
// certification it could not reach as in force.
type SweepResult struct {
	LegalEntityID string    `json:"legal_entity_id"`
	Checked       int       `json:"checked"`
	Withdrawn     int       `json:"withdrawn"`
	NewFlags      int       `json:"new_flags"`
	Failed        int       `json:"failed"`
	Outcomes      []Outcome `json:"outcomes"`
}

// Sweeper checks the watched certifications of a legal entity.
type Sweeper struct {
	Store  Store
	Status StatusReader
}

// SweepOne checks one watched certification with the service that issued it and acts on the answer. It is the unit of every
// route into the watch (a manual sweep, the schedule, a withdrawal event), so they cannot disagree about what a sweep is:
//
//   - one that cannot be checked is reported (Error set) and never recorded as in force;
//   - one found REVOKED is marked withdrawn and everything citing it is flagged;
//   - one already withdrawn is NOT asked about again but IS re-scanned, because a journal can be posted after its certification
//     was marked withdrawn (it recorded its reliance before the revocation and finalized after the last sweep).
func (s *Sweeper) SweepOne(ctx context.Context, tenant, principal, correlationID string, w Watched) Outcome {
	out := Outcome{Certification: w.Ref.String()}
	if w.WithdrawnAt != nil {
		out.State, out.Withdrawn = certreliance.StateRevoked, true
		n, err := s.Store.RecordWithdrawal(ctx, tenant, w.LegalEntityID, w.Ref, w.RevocationID, w.Reason)
		if err != nil {
			out.Error = err.Error()
		}
		out.NewFlags = n
		return out
	}
	st, err := s.Status.CertificationStatus(ctx, tenant, principal, correlationID, w.Ref)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.State = st.State
	if err := s.Store.RecordCheck(ctx, tenant, w.LegalEntityID, w.Ref, st.State); err != nil {
		out.Error = err.Error()
		return out
	}
	if st.State == certreliance.StateRevoked {
		out.Withdrawn = true
		n, err := s.Store.RecordWithdrawal(ctx, tenant, w.LegalEntityID, w.Ref, st.RevocationID, st.Reason)
		if err != nil {
			out.Error = err.Error()
		}
		out.NewFlags = n
	}
	return out
}

// Sweep checks every certification the entity relies on (SweepOne for each). One that fails does not stop the others.
func (s *Sweeper) Sweep(ctx context.Context, tenant, principal, correlationID, legalEntityID string) (*SweepResult, error) {
	watched, err := s.Store.ListWatched(ctx, tenant, legalEntityID)
	if err != nil {
		return nil, err
	}
	res := &SweepResult{LegalEntityID: legalEntityID, Outcomes: []Outcome{}}
	for _, w := range watched {
		o := s.SweepOne(ctx, tenant, principal, correlationID, w)
		res.Checked++
		res.NewFlags += o.NewFlags
		if o.Withdrawn {
			res.Withdrawn++
		}
		if o.Error != "" {
			res.Failed++
		}
		res.Outcomes = append(res.Outcomes, o)
	}
	return res, nil
}
