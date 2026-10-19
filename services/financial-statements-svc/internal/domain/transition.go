package domain

// machine is the lifecycle table, keyed by command then current state.
// Anything not in the table is INVALID_TRANSITION.
//
//	DRAFT      --VALIDATE_STATEMENTS-->  VALIDATED   (only when tie-outs pass;
//	                                                   a failing validation
//	                                                   stays DRAFT with
//	                                                   ValidationErrors set —
//	                                                   see service.Validate)
//	VALIDATED  --SUBMIT_FOR_REVIEW-->    IN_REVIEW
//	IN_REVIEW  --APPROVE_STATEMENT_SET--> APPROVED
//	IN_REVIEW  --REQUEST_CHANGES-->      DRAFT        (spec addition, see
//	                                                   SPEC_DEVIATIONS.md B2:
//	                                                   the spec's own state
//	                                                   model has no reviewer
//	                                                   rejection path)
//	APPROVED   --CERTIFY_STATEMENT_SET--> CERTIFIED
//	CERTIFIED  --PUBLISH_STATEMENT_SET--> PUBLISHED
//
// REFRESH_DRAFT is a self-loop from DRAFT (regenerate facts from the same
// snapshot) and is also how VALIDATED/IN_REVIEW/APPROVED fall back to DRAFT if
// anyone forces a refresh after validation — but RefreshDraft is only ever
// invoked explicitly from DRAFT in this version (see service.RefreshDraft);
// the other states reach DRAFT only via REQUEST_CHANGES.
//
// ADD_DISCLOSURE is a DRAFT self-loop for the same reason REFRESH_DRAFT is:
// report content — facts AND disclosures — only ever changes in DRAFT. A
// VALIDATED, IN_REVIEW or APPROVED set is content someone has checked or is
// checking; changing it means REQUEST_CHANGES back to DRAFT first.
//
// SUPERSEDE is deliberately not in this table: it is a two-aggregate
// operation (old set -> SUPERSEDED, a brand NEW set created in DRAFT), driven
// by CanSupersede below, not Next.
var machine = map[Command]map[State]State{
	CmdRefreshDraft:    {StateDraft: StateDraft},
	CmdValidate:        {StateDraft: StateValidated},
	CmdSubmitForReview: {StateValidated: StateInReview},
	CmdRequestChanges:  {StateInReview: StateDraft},
	CmdApprove:         {StateInReview: StateApproved},
	CmdCertify:         {StateApproved: StateCertified},
	CmdPublish:         {StateCertified: StatePublished},
	CmdAddDisclosure:   {StateDraft: StateDraft},
}

// Next returns the state a command leads to from `from`, or INVALID_TRANSITION.
func Next(cmd Command, from State) (State, error) {
	t, ok := machine[cmd]
	if !ok {
		return "", Errf(CodeInvalidTransition, "unknown statement set command %q", cmd)
	}
	if !from.Valid() {
		return "", Errf(CodeInvalidTransition, "unknown statement set state %q", from)
	}
	to, ok := t[from]
	if !ok {
		return "", Errf(CodeInvalidTransition, "command %s is not allowed from state %s", cmd, from)
	}
	return to, nil
}

// CanSupersede reports whether a statement set in this state may be
// superseded by a new one (spec: certified output is immutable; a correction
// is always a NEW set, never an edit of the old one).
func CanSupersede(s State) bool {
	return s == StateCertified || s == StatePublished
}
