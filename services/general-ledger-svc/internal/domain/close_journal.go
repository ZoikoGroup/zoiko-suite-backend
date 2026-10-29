package domain

// Close-journal designations are ACC-14's "designated close journals/workflows"
// for the CLOSE_REVIEW posting policy. The specification names the category but
// does not enumerate it, so this service treats it as a closed, server-validated
// allowlist of the close workflows the platform actually runs. A caller may
// declare one; it cannot invent one, and the declaration is only honored when
// the journal's ACC-03 type agrees with it.
//
// The values are deliberately not a boolean and not the journal type itself:
// every one of these workflows posts an ACCRUAL journal, so journal_type alone
// cannot distinguish a close journal from an ordinary accrual.
const (
	// CloseJournalDesignationAccrualRecognition is ACC-07's period-end accrual
	// recognition.
	CloseJournalDesignationAccrualRecognition = "ACCRUAL_RECOGNITION"

	// CloseJournalDesignationPrepaymentRecognition is ACC-08's prepayment /
	// deferral recognition.
	CloseJournalDesignationPrepaymentRecognition = "PREPAYMENT_RECOGNITION"

	// CloseJournalDesignationPrepaymentTermination is ACC-08's final
	// settlement when a prepayment schedule is terminated.
	CloseJournalDesignationPrepaymentTermination = "PREPAYMENT_TERMINATION"
)

// ValidCloseJournalDesignation reports whether designation is a recognised
// close workflow and whether journalType is the type that workflow posts.
// The empty string is valid and means "not a designated close journal".
func ValidCloseJournalDesignation(designation string, journalType JournalType) bool {
	if designation == "" {
		return true
	}
	switch designation {
	case CloseJournalDesignationAccrualRecognition,
		CloseJournalDesignationPrepaymentRecognition,
		CloseJournalDesignationPrepaymentTermination:
		return journalType == JournalTypeAccrual
	default:
		return false
	}
}
