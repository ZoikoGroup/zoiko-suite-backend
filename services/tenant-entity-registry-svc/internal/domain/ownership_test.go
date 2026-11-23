package domain

import (
	"errors"
	"testing"
	"time"
)

func validOwnership() ProposeOwnershipRequest {
	pct := 60.0
	return ProposeOwnershipRequest{
		ParentLegalEntityID: "11111111-1111-1111-1111-111111111111",
		ChildLegalEntityID:  "22222222-2222-2222-2222-222222222222",
		RelationshipKind:    RelationshipOwnership,
		OwnershipPct:        &pct,
		ValidFrom:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EvidenceRef:         "filing-2026-001",
		Reason:              "acquisition",
	}
}

func TestProposeOwnershipRequest_Validate(t *testing.T) {
	if err := validOwnership().Validate(); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	zero, over := 0.0, 100.0001
	unknown := ControlType("FRIENDSHIP")
	board := ControlBoard
	before := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := map[string]func(*ProposeOwnershipRequest){
		"no parent":             func(r *ProposeOwnershipRequest) { r.ParentLegalEntityID = "" },
		"self ownership":        func(r *ProposeOwnershipRequest) { r.ChildLegalEntityID = r.ParentLegalEntityID },
		"unknown kind":          func(r *ProposeOwnershipRequest) { r.RelationshipKind = "FRIENDSHIP" },
		"ownership without pct": func(r *ProposeOwnershipRequest) { r.OwnershipPct = nil },
		"control without type": func(r *ProposeOwnershipRequest) {
			r.RelationshipKind, r.OwnershipPct = RelationshipControl, nil
		},
		"unknown control type": func(r *ProposeOwnershipRequest) { r.ControlType = &unknown },
		"zero pct":             func(r *ProposeOwnershipRequest) { r.OwnershipPct = &zero },
		"pct over 100":         func(r *ProposeOwnershipRequest) { r.OwnershipPct = &over },
		"voting pct over 100":  func(r *ProposeOwnershipRequest) { r.VotingPct = &over },
		"no valid_from":        func(r *ProposeOwnershipRequest) { r.ValidFrom = time.Time{} },
		"valid_to not after":   func(r *ProposeOwnershipRequest) { r.ValidTo = &before },
		"no evidence":          func(r *ProposeOwnershipRequest) { r.EvidenceRef = "  " },
		"no reason":            func(r *ProposeOwnershipRequest) { r.Reason = "" },
	}
	for name, mutate := range cases {
		r := validOwnership()
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidOwnership) {
			t.Errorf("%s: want ErrInvalidOwnership, got %v", name, err)
		}
	}

	// A CONTROL relationship with a type and no equity is valid.
	c := validOwnership()
	c.RelationshipKind, c.OwnershipPct, c.ControlType = RelationshipControl, nil, &board
	if err := c.Validate(); err != nil {
		t.Errorf("control without equity refused: %v", err)
	}
}
