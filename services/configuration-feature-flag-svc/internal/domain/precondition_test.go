package domain

import (
	"context"
	"errors"
	"testing"
)

func TestCFG1_CheckPrecondition(t *testing.T) {
	bg := context.Background()
	cases := []struct {
		name    string
		ctx     context.Context
		current int64
		stale   bool
	}{
		{"no precondition passes", bg, 7, false},
		{"matching version", WithPrecondition(bg, Precondition{Version: 3}), 3, false},
		{"older version is stale", WithPrecondition(bg, Precondition{Version: 2}), 3, true},
		{"newer version is stale", WithPrecondition(bg, Precondition{Version: 4}), 3, true},
		{"version against nothing is stale", WithPrecondition(bg, Precondition{Version: 1}), 0, true},
		{"any holds when it exists", WithPrecondition(bg, Precondition{Any: true}), 5, false},
		{"any fails when nothing exists", WithPrecondition(bg, Precondition{Any: true}), 0, true},
		{"none-match holds when nothing exists", WithPrecondition(bg, Precondition{NoneMatch: true}), 0, false},
		{"none-match fails when it exists", WithPrecondition(bg, Precondition{NoneMatch: true}), 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPrecondition(tc.ctx, tc.current)
			var stale *StaleVersionError
			if tc.stale {
				if !errors.As(err, &stale) {
					t.Fatalf("want StaleVersionError, got %v", err)
				}
				if stale.Current != tc.current {
					t.Fatalf("Current = %d, want %d", stale.Current, tc.current)
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}
