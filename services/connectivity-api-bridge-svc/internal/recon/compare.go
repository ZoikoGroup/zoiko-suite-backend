package recon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

const (
	maxRefs         = 50000
	maxAttemptItems = 200
	sampleSize      = 50
)

// finding is a difference before it is stored as a variance.
type finding struct {
	Type     string
	Severity string
	Key      string // what makes the finding the same finding on the next run
	Refs     map[string]any
}

func (f finding) fingerprint(connectionID string) string {
	h := sha256.Sum256([]byte(connectionID + "\x00" + f.Type + "\x00" + f.Key))
	return hex.EncodeToString(h[:])
}

// outcome is everything one evaluation produced.
type outcome struct {
	Findings  []finding
	Result    map[string]any
	Truncated bool
}

func parseAmount(s string) (*big.Rat, bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	return r, ok
}

func fmtAmount(r *big.Rat) string {
	s := r.FloatString(6)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

func sevCount(p Policy, diff int) string {
	switch {
	case diff >= p.HighCountDelta:
		return SevHigh
	case diff > p.MediumCountDelta:
		return SevMedium
	}
	return SevLow
}

func setHash(items []string) string {
	s := append([]string(nil), items...)
	sort.Strings(s)
	h := sha256.Sum256([]byte(strings.Join(s, "\x00")))
	return hex.EncodeToString(h[:])
}

func sample(items []string) []string {
	s := append([]string(nil), items...)
	sort.Strings(s)
	if len(s) > sampleSize {
		s = s[:sampleSize]
	}
	return s
}

// compare evaluates a run against the platform's real records. The provider's declaration (ext, dom) is one side and
// Facts is the other: nothing here derives one side from the other.
func compare(ctx context.Context, facts Facts, p Policy, run *Run, ext *External, dom *DomainTotals, now time.Time) (*outcome, error) {
	out := &outcome{Result: map[string]any{"domain": p.Domain}}
	checked := map[string]any{}
	conn := run.ConnectionID
	has := func(b string) bool {
		for _, x := range run.Basis {
			if x == b {
				return true
			}
		}
		return false
	}

	if has(BasisCount) && ext != nil {
		in, err := facts.InboundCounts(ctx, conn, run.PeriodFrom, run.PeriodTo)
		if err != nil {
			return nil, err
		}
		ob, err := facts.OutboundCounts(ctx, conn, run.PeriodFrom, run.PeriodTo)
		if err != nil {
			return nil, err
		}
		internal := map[string]int{}
		for k, v := range in {
			internal[k] = v
		}
		for k, v := range ob {
			internal[k] = v
		}
		names := make([]string, 0, len(ext.Counts))
		for n := range ext.Counts {
			names = append(names, n)
		}
		sort.Strings(names)
		rows := []map[string]any{}
		for _, n := range names {
			e, i := ext.Counts[n], internal[n]
			d := e - i
			if d < 0 {
				d = -d
			}
			rows = append(rows, map[string]any{"name": n, "external": e, "internal": i, "difference": e - i})
			if d > p.CountTolerance {
				out.Findings = append(out.Findings, finding{Type: VarCount, Severity: sevCount(p, d), Key: n,
					Refs: map[string]any{"name": n, "external": e, "internal": i, "difference": e - i}})
			}
		}
		checked[BasisCount] = rows
	}

	if has(BasisReference) && ext != nil {
		have, err := facts.References(ctx, conn, run.PeriodFrom, run.PeriodTo, maxRefs+1)
		if err != nil {
			return nil, err
		}
		if len(have) > maxRefs {
			out.Truncated = true
			checked[BasisReference] = map[string]any{"skipped": "the platform holds more references than one run compares"}
		} else {
			internal := map[string]bool{}
			for _, r := range have {
				internal[r] = true
			}
			external := map[string]bool{}
			for _, r := range ext.References {
				external[r] = true
			}
			var missInt, missExt []string
			for r := range external {
				if !internal[r] {
					missInt = append(missInt, r)
				}
			}
			for r := range internal {
				if !external[r] {
					missExt = append(missExt, r)
				}
			}
			checked[BasisReference] = map[string]any{"external": len(external), "internal": len(internal), "missing_internal": len(missInt), "missing_external": len(missExt)}
			add := func(typ string, items []string, sev string) {
				if len(items) == 0 {
					return
				}
				out.Findings = append(out.Findings, finding{Type: typ, Severity: sev, Key: setHash(items),
					Refs: map[string]any{"count": len(items), "sample": sample(items), "set_hash": setHash(items)}})
			}
			sev := SevMedium
			if len(missInt) >= p.HighCountDelta {
				sev = SevHigh
			}
			add(VarRefMissingInt, missInt, sev)
			sev = SevMedium
			if len(missExt) >= p.HighCountDelta {
				sev = SevHigh
			}
			add(VarRefMissingExt, missExt, sev)
		}
	}

	if has(BasisAmount) && ext != nil && dom != nil {
		er, ok1 := parseAmount(ext.TotalAmount)
		dr, ok2 := parseAmount(dom.TotalAmount)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%w: a total is not a decimal", ErrBadRequest)
		}
		diff := new(big.Rat).Sub(er, dr)
		abs := new(big.Rat).Abs(diff)
		row := map[string]any{"external_total": fmtAmount(er), "domain_total": fmtAmount(dr), "difference": fmtAmount(diff), "currency": ext.Currency}
		checked[BasisAmount] = row
		if !strings.EqualFold(ext.Currency, dom.Currency) {
			out.Findings = append(out.Findings, finding{Type: VarAmount, Severity: SevHigh, Key: "currency:" + ext.Currency + ":" + dom.Currency,
				Refs: map[string]any{"external_currency": ext.Currency, "domain_currency": dom.Currency}})
		} else if abs.Sign() != 0 {
			base := new(big.Rat).Abs(er)
			bp := func(n int) *big.Rat { return new(big.Rat).Mul(base, big.NewRat(int64(n), 10000)) }
			if abs.Cmp(bp(p.AmountToleranceBP)) > 0 {
				sev := SevMedium
				if base.Sign() == 0 || abs.Cmp(bp(p.HighAmountBP)) >= 0 {
					sev = SevHigh
				}
				out.Findings = append(out.Findings, finding{Type: VarAmount, Severity: sev, Key: "amount:" + ext.Currency, Refs: row})
			}
		}
	}

	if has(BasisState) || has(BasisEvidence) {
		if has(BasisState) {
			at, err := facts.Attempts(ctx, conn, run.PeriodFrom, run.PeriodTo, maxAttemptItems+1)
			if err != nil {
				return nil, err
			}
			if len(at) > maxAttemptItems {
				at = at[:maxAttemptItems]
				out.Truncated = true
			}
			n := 0
			for _, a := range at {
				switch {
				case a.State == "UNKNOWN" && now.Sub(a.UpdatedAt) > p.UnknownMaxAge:
					// NP-46: escalated, never forced to FAILED to clear the queue
					out.Findings = append(out.Findings, finding{Type: VarUnknown, Severity: SevHigh, Key: a.AttemptID,
						Refs: map[string]any{"attempt_id": a.AttemptID, "age_seconds": int(now.Sub(a.UpdatedAt).Seconds()), "limit_seconds": int(p.UnknownMaxAge.Seconds()), "escalated": a.EscalatedAt != nil}})
					n++
				case (a.State == "PENDING" || a.State == "SUBMITTING") && now.Sub(a.UpdatedAt) > p.PendingMaxAge:
					out.Findings = append(out.Findings, finding{Type: VarStalePending, Severity: SevMedium, Key: a.AttemptID,
						Refs: map[string]any{"attempt_id": a.AttemptID, "state": a.State, "age_seconds": int(now.Sub(a.UpdatedAt).Seconds())}})
					n++
				case a.NeedsReconcile:
					out.Findings = append(out.Findings, finding{Type: VarLateContradicts, Severity: SevHigh, Key: a.AttemptID,
						Refs: map[string]any{"attempt_id": a.AttemptID, "xic_state": a.State, "provider_state": a.LatestReceiptState}})
					n++
				case (a.State == "SETTLED" || a.State == "FAILED") && (a.LatestReceiptState == "SETTLED" || a.LatestReceiptState == "FAILED") && a.State != a.LatestReceiptState:
					out.Findings = append(out.Findings, finding{Type: VarState, Severity: SevHigh, Key: a.AttemptID,
						Refs: map[string]any{"attempt_id": a.AttemptID, "xic_state": a.State, "provider_state": a.LatestReceiptState}})
					n++
				}
			}
			checked[BasisState] = map[string]any{"attempts": len(at), "findings": n}
		}
		if has(BasisEvidence) {
			bs, err := facts.Batches(ctx, conn, run.PeriodFrom, run.PeriodTo, maxAttemptItems+1)
			if err != nil {
				return nil, err
			}
			if len(bs) > maxAttemptItems {
				bs = bs[:maxAttemptItems]
				out.Truncated = true
			}
			n := 0
			for _, b := range bs {
				if b.VarianceRows > 0 || (b.DeclaredRows != nil && b.ProcessingDone && *b.DeclaredRows != b.AdmittedRows+b.RejectedRows) {
					refs := map[string]any{"batch_id": b.BatchID, "external_batch_id": b.ExternalBatchID, "admitted_rows": b.AdmittedRows, "rejected_rows": b.RejectedRows, "variance_rows": b.VarianceRows}
					if b.DeclaredRows != nil {
						refs["declared_rows"] = *b.DeclaredRows
					}
					out.Findings = append(out.Findings, finding{Type: VarManifest, Severity: SevMedium, Key: b.BatchID, Refs: refs})
					n++
				}
				if b.State != "RECONCILED" && b.State != "REJECTED" && b.State != "QUARANTINED" && now.Sub(b.ReceivedAt) > p.BatchLag {
					out.Findings = append(out.Findings, finding{Type: VarBatchIncomplete, Severity: SevMedium, Key: b.BatchID,
						Refs: map[string]any{"batch_id": b.BatchID, "external_batch_id": b.ExternalBatchID, "state": b.State, "pending_rows": b.PendingRows, "age_seconds": int(now.Sub(b.ReceivedAt).Seconds())}})
					n++
				}
			}
			checked[BasisEvidence] = map[string]any{"batches": len(bs), "findings": n}
		}
	}

	if has(BasisTime) && ext != nil && ext.Cutoff != nil {
		n, err := facts.LateInbound(ctx, conn, run.PeriodFrom, run.PeriodTo, *ext.Cutoff)
		if err != nil {
			return nil, err
		}
		checked[BasisTime] = map[string]any{"cutoff": ext.Cutoff.UTC().Format(time.RFC3339), "late": n}
		if n > p.CountTolerance {
			out.Findings = append(out.Findings, finding{Type: VarLateArrival, Severity: sevCount(p, n), Key: "cutoff:" + ext.Cutoff.UTC().Format(time.RFC3339),
				Refs: map[string]any{"late": n, "cutoff": ext.Cutoff.UTC().Format(time.RFC3339)}})
		}
	}

	out.Result["checked"] = checked
	out.Result["findings"] = len(out.Findings)
	if out.Truncated {
		out.Result["truncated"] = true
	}
	return out, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
