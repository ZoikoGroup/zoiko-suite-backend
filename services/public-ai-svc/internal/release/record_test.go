package release

import (
	"errors"
	"testing"

	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 08 §06/§33, OPR-048, Doc 11 §24: a manifest is recorded immutably,
// verified at deploy, and carries named sign-offs.
func TestManifestRecordAndSignoffs(t *testing.T) {
	st := store.NewMemory()
	pub, priv, _ := signing.GenerateKey()
	body := []byte(`{"release_id":"r0-1"}`)
	sig, _ := signing.Sign(priv, body)
	op := Operator{Actor: "rel-mgr", Approval: "CHG-10", Environment: "staging"}

	if _, _, err := VerifyDeployManifest(ctx, st, pub, "r0-1", body, sig); !errors.Is(err, ErrManifestNotRecorded) {
		t.Fatalf("unrecorded manifest: %v", err)
	}
	if err := SignOff(ctx, st, op, "sre", "r0-1", body); !errors.Is(err, ErrManifestNotRecorded) {
		t.Fatalf("sign-off before recording: %v", err)
	}
	if err := RecordManifest(ctx, st, op, "r0-1", body); err != nil {
		t.Fatal(err)
	}
	if err := SignOff(ctx, st, Operator{Actor: "sre-lead", Approval: "CHG-10"}, "sre", "r0-1", body); err != nil {
		t.Fatal(err)
	}
	if err := SignOff(ctx, st, op, "janitor", "r0-1", body); err == nil {
		t.Fatal("an unknown role must be refused")
	}
	signed, missing, err := VerifyDeployManifest(ctx, st, pub, "r0-1", body, sig)
	if err != nil || len(signed) != 1 || signed[0].Role != "sre" || signed[0].Actor != "sre-lead" || len(missing) != len(SignoffRoles)-1 {
		t.Fatalf("signed %+v missing %v err %v", signed, missing, err)
	}
	edited := []byte(`{"release_id":"r0-1","decision":"GO"}`)
	editedSig, _ := signing.Sign(priv, edited)
	if _, _, err := VerifyDeployManifest(ctx, st, pub, "r0-1", edited, editedSig); !errors.Is(err, ErrManifestNotRecorded) {
		t.Fatalf("a manifest changed after recording must not verify: %v", err)
	}
	if _, _, err := VerifyDeployManifest(ctx, st, pub, "r0-1", body, editedSig); err == nil {
		t.Fatal("a bad signature must not verify")
	}
}
