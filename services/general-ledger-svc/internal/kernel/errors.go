// Package kernel is the Global Accounting Kernel (ZoikoSuite Global Accounting Kernel standard): the books, the
// immutable accounting events, the versioned posting rules and the deterministic evaluation that turns an event into
// a journal proposal. Evaluation never writes: it returns a proposal with its provenance, and the posting core
// (separate, atomic) is the only thing that creates ledger consequences.
package kernel

import (
	"errors"
	"fmt"
)

// Families of the posting exception taxonomy (standard §34). A family tells the caller what must happen next: every one
// of them means "no posting".
const (
	FamAuth   = "AK-AUTH"
	FamPeriod = "AK-PERIOD"
	FamRule   = "AK-RULE"
	FamCOA    = "AK-COA"
	FamDim    = "AK-DIM"
	FamBal    = "AK-BAL"
	FamFX     = "AK-FX"
	FamTax    = "AK-TAX"
	FamDup    = "AK-DUP"
	FamConc   = "AK-CONC"
	FamExt    = "AK-EXT"
	FamData   = "AK-DATA"
)

// Exception is a deterministic refusal with the facts that explain it. It is data, not a crash: the caller records it.
type Exception struct {
	Family string         `json:"family"`
	Code   string         `json:"code"`
	Detail string         `json:"detail"`
	Facts  map[string]any `json:"facts,omitempty"`
}

func (e *Exception) Error() string { return e.Family + "/" + e.Code + ": " + e.Detail }

func exc(family, code, format string, a ...any) *Exception {
	return &Exception{Family: family, Code: code, Detail: fmt.Sprintf(format, a...)}
}

func (e *Exception) with(k string, v any) *Exception {
	if e.Facts == nil {
		e.Facts = map[string]any{}
	}
	e.Facts[k] = v
	return e
}

// AsException unwraps an *Exception.
func AsException(err error) (*Exception, bool) {
	var e *Exception
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Sentinel errors of the stores.
var (
	ErrNotFound     = errors.New("not found")
	ErrExists       = errors.New("already exists")
	ErrState        = errors.New("the object is not in a state that allows this")
	ErrVersion      = errors.New("version conflict: the object changed since it was read")
	ErrSelfApproval = errors.New("that step must be taken by someone other than the person who prepared it")
	ErrIdempotency  = errors.New("idempotency key was used for a different request")
	ErrOverlap      = errors.New("an active rule set already covers this event type, book scope and period")
	ErrNotTested    = errors.New("the rule set's test pack has not passed for this exact content")
	ErrBadRequest   = errors.New("bad request")
)

// NewException builds an exception for a collaborator outside this package (the period gate), so every refusal reaches
// the caller in the one AK-* taxonomy.
func NewException(family, code, format string, a ...any) *Exception { return exc(family, code, format, a...) }
