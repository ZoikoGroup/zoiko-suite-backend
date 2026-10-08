package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
)

// FingerprintVersion is bumped whenever the set of fields a fingerprint
// covers changes. It is part of the hashed content, so a fingerprint from
// another version can never compare equal.
const FingerprintVersion = 2

func cents(v float64) int64 { return int64(math.Round(v * 100)) }

// ComputeFingerprint is the proposal's subject fingerprint (spec §16
// "subject_fingerprint"): a SHA-256 over everything an authorization binds
// — the payable population, amounts, payee versions, payer account,
// currency, method and value date. Changing any of them changes the
// fingerprint, which invalidates an authorization taken against the old one
// (invariant #3).
//
// status is a parameter because the fingerprint stored at freeze is
// computed while the row is still in REVIEW but must equal the live value
// once it is FROZEN; the caller passes the status the proposal will have.
// Only active items are covered, in a stable order, with every string
// quoted so no field can bleed into the next. Money is hashed in cents so
// float formatting can never change the digest.
//
// The payee "version" covered is each item's PayeeSnapshotAt (the supplier
// profile's updated_at when the item was added). The ORG-10 beneficiary
// destination is pinned separately by payment-authorization-svc at
// approval time.
func ComputeFingerprint(p *PaymentProposal, items []ProposalItem, status ProposalStatus) string {
	active := make([]ProposalItem, 0, len(items))
	for _, it := range items {
		if it.IsActive {
			active = append(active, it)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ItemID < active[j].ItemID })

	var b strings.Builder
	fmt.Fprintf(&b, "v%d|%q|%q|%q|%q|%q|%s|%q|%d|%d|%d",
		FingerprintVersion, p.ProposalID, status, p.LegalEntityID, p.PayingBankAccountRef, p.Currency,
		p.PaymentDate.UTC().Format("2006-01-02"), p.PaymentMethod,
		cents(p.GrossAmount), cents(p.WithholdingAmount), cents(p.NetAmount))
	for _, it := range active {
		snapshot := ""
		if it.PayeeSnapshotAt != nil {
			snapshot = it.PayeeSnapshotAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
		}
		fmt.Fprintf(&b, "\n%q|%q|%q|%q|%d|%d|%d|%q|%s|%s|%q|%q",
			it.ItemID, it.PayableSource, it.PayableID, it.PayeeRef,
			cents(it.GrossAmount), cents(it.WithholdingAmount), cents(it.NetAmount),
			it.Currency, it.DueDate.UTC().Format("2006-01-02"), snapshot, it.TaxDeterminationID, it.ExceptionRef)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}
