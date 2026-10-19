package domain

import (
	"errors"
	"testing"
)

func TestLifecycle_EdgesAreClosed(t *testing.T) {
	legal := []struct {
		from, to RunStatus
		result   ResultState
		cert     CertificationState
	}{
		{RunPlanned, RunRunning, ResultNotRun, CertUncertified},
		{RunPlanned, RunFailed, ResultIndeterminate, CertUncertified},
		{RunRunning, RunRunning, ResultMatch, CertEligible},
		{RunRunning, RunExceptionsOpen, ResultVariance, CertUncertified},
		{RunExceptionsOpen, RunRunning, ResultMatch, CertEligible},
		{RunRunning, RunReperformed, ResultVariance, CertUncertified},
		{RunReperformed, RunCertified, ResultMatch, CertCertified},
		{RunCertified, RunSuperseded, ResultSuperseded, CertSuperseded},
		{RunFailed, RunSuperseded, ResultSuperseded, CertUncertified},
	}
	for _, c := range legal {
		if err := ValidateLifecycleTransition(c.from, c.to, c.result, c.cert, false); err != nil {
			t.Errorf("%s -> %s (%s/%s) refused: %v", c.from, c.to, c.result, c.cert, err)
		}
	}
	illegal := []struct {
		name     string
		from, to RunStatus
		result   ResultState
		cert     CertificationState
	}{
		{"skip evaluation", RunPlanned, RunCertified, ResultMatch, CertCertified},
		{"planned to exceptions", RunPlanned, RunExceptionsOpen, ResultVariance, CertUncertified},
		{"certified edited", RunCertified, RunRunning, ResultMatch, CertEligible},
		{"superseded revived", RunSuperseded, RunRunning, ResultMatch, CertEligible},
		{"failed with a match", RunPlanned, RunFailed, ResultMatch, CertUncertified},
		{"certify a variance", RunRunning, RunCertified, ResultVariance, CertCertified},
		{"eligible variance", RunRunning, RunRunning, ResultVariance, CertEligible},
		{"certified without the state", RunRunning, RunCertified, ResultMatch, CertEligible},
		{"certification state without the status", RunRunning, RunRunning, ResultMatch, CertCertified},
		{"exceptions open but matching", RunRunning, RunExceptionsOpen, ResultMatch, CertEligible},
		{"superseded keeps its result", RunCertified, RunSuperseded, ResultMatch, CertSuperseded},
		{"superseded result on an open run", RunRunning, RunRunning, ResultSuperseded, CertUncertified},
	}
	for _, c := range illegal {
		if err := ValidateLifecycleTransition(c.from, c.to, c.result, c.cert, false); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: %v, want ErrInvalidTransition", c.name, err)
		}
	}
}

func TestResult_Transitions(t *testing.T) {
	for _, c := range []struct {
		name   string
		status RunStatus
		from   ResultState
		to     ResultState
		ok     bool
	}{
		{"first evaluation", RunRunning, ResultNotRun, ResultVariance, true},
		{"resolved", RunExceptionsOpen, ResultVariance, ResultMatch, true},
		{"new exception", RunRunning, ResultMatch, ResultVariance, true},
		{"source failure", RunPlanned, ResultNotRun, ResultIndeterminate, true},
		{"supersede a failure", RunFailed, ResultIndeterminate, ResultSuperseded, true},
		{"unchanged", RunRunning, ResultMatch, ResultMatch, true},
		{"INV-05: indeterminate to match", RunRunning, ResultIndeterminate, ResultMatch, false},
		{"reset", RunRunning, ResultMatch, ResultNotRun, false},
		{"partial has no semantics", RunRunning, ResultMatch, ResultPartial, false},
		{"certified result frozen", RunCertified, ResultMatch, ResultVariance, false},
		{"failed result frozen", RunFailed, ResultIndeterminate, ResultVariance, false},
	} {
		err := ValidateResultTransition(c.status, c.from, c.to)
		if c.ok != (err == nil) {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestCertification_Transitions(t *testing.T) {
	ok := [][2]CertificationState{
		{CertUncertified, CertEligible}, {CertEligible, CertUncertified}, {CertEligible, CertCertified},
		{CertCertified, CertSuperseded}, {CertUncertified, CertUncertified},
	}
	for _, c := range ok {
		if err := ValidateCertificationTransition(c[0], c[1]); err != nil {
			t.Errorf("%s -> %s: %v", c[0], c[1], err)
		}
	}
	bad := [][2]CertificationState{
		{CertUncertified, CertCertified}, {CertCertified, CertEligible}, {CertCertified, CertUncertified},
		{CertSuperseded, CertCertified}, {CertUncertified, CertSuperseded},
	}
	for _, c := range bad {
		if err := ValidateCertificationTransition(c[0], c[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s: %v, want ErrInvalidTransition", c[0], c[1], err)
		}
	}
}

func TestEvaluatedResult(t *testing.T) {
	if EvaluatedResult(0) != ResultMatch || CertificationFor(RunAssessment{Certifiable: true}) != CertEligible {
		t.Fatal("no open exceptions must be MATCH / ELIGIBLE")
	}
	if EvaluatedResult(1) != ResultVariance || CertificationFor(RunAssessment{}) != CertUncertified {
		t.Fatal("an open exception must be VARIANCE / UNCERTIFIED")
	}
}

func TestRunFailure_Validate(t *testing.T) {
	if err := (RunFailure{Code: FailureSourceUnavailable, Reason: "down"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (RunFailure{Code: "OOPS", Reason: "x"}).Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown code: %v", err)
	}
	if err := (RunFailure{Code: FailureSourceUnavailable, Reason: " "}).Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank reason: %v", err)
	}
}
