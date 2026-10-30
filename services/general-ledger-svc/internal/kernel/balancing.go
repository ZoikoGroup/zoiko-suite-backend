package kernel

import (
	"context"
	"math/big"
	"sort"
)

// Balancing dimension (standard §13): where a rule designates a dimension, the journal must balance within each value of it, or carry
// explicit due-to and due-from lines that make it so. A value whose lines debit more than they credit gets a credit on the due-to
// account; one that credits more gets a debit on the due-from account, so the two sides of the transfer between values are visible and
// the journal as a whole is unchanged. Without those accounts an imbalance within a value is refused, never absorbed.

// BalancingDimension designates the dimension a journal must balance by.
type BalancingDimension struct {
	Dimension string `json:"dimension"`
	// DueTo is credited by a value whose lines debit more than they credit; DueFrom is debited by one that credits more. Both or neither.
	DueTo   *AccountRef `json:"due_to_account,omitempty"`
	DueFrom *AccountRef `json:"due_from_account,omitempty"`
}

func (b *BalancingDimension) validate() string {
	if !codeRE.MatchString(b.Dimension) {
		return "a dimension code"
	}
	if (b.DueTo == nil) != (b.DueFrom == nil) {
		return "both a due-to and a due-from account, or neither"
	}
	if b.DueTo != nil && (!b.DueTo.valid() || !b.DueFrom.valid()) {
		return "each account is exactly one of account_code or mapping_key"
	}
	return ""
}

type dimGroup struct {
	key       string
	tx, fn    *big.Int // debit minus credit, in transaction and functional minor units
	hasLines  bool
	imbalance bool
}

// balanceByDimension makes every value of the balancing dimension balance, adding due-to and due-from lines where the rule provides the
// accounts for them and refusing the journal where it does not. It returns the lines it added.
func balanceByDimension(ctx context.Context, in EvalInput, c RuleContent, lines *[]*rawLine) ([]string, error) {
	cfg := c.BalancingDimension
	if cfg == nil {
		return nil, nil
	}
	groups := map[string]*dimGroup{}
	for _, l := range *lines {
		k := l.dims[cfg.Dimension]
		g := groups[k]
		if g == nil {
			g = &dimGroup{key: k, tx: new(big.Int), fn: new(big.Int)}
			groups[k] = g
		}
		g.hasLines = true
		if l.t.Side == Debit {
			g.tx.Add(g.tx, l.minor)
			g.fn.Add(g.fn, l.funcMn)
		} else {
			g.tx.Sub(g.tx, l.minor)
			g.fn.Sub(g.fn, l.funcMn)
		}
	}
	keys := make([]string, 0, len(groups))
	for k, g := range groups {
		if g.tx.Sign() != 0 || g.fn.Sign() != 0 {
			g.imbalance = true
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Strings(keys)
	label := func(k string) string {
		if k == "" {
			return "(no " + cfg.Dimension + ")"
		}
		return cfg.Dimension + "=" + k
	}
	if cfg.DueTo == nil {
		g := groups[keys[0]]
		return nil, exc(FamBal, "DIMENSION_UNBALANCED", "the journal does not balance within %s (net %s minor units) and the rule has no due-to or due-from account to carry the difference",
			label(keys[0]), g.fn.String()).with("dimension", cfg.Dimension).with("value", keys[0])
	}
	resolve := func(ref AccountRef) (*AccountInfo, error) {
		info, err := in.Directory.ResolveAccount(ctx, ref, in.Book.BookID, in.PostingDate)
		if err != nil || info == nil || !info.Active || !info.Postable {
			return nil, exc(FamCOA, "BALANCING_ACCOUNT_UNAVAILABLE", "the due-to or due-from account %s cannot take the difference", ref)
		}
		return info, nil
	}
	dueTo, err := resolve(*cfg.DueTo)
	if err != nil {
		return nil, err
	}
	dueFrom, err := resolve(*cfg.DueFrom)
	if err != nil {
		return nil, err
	}
	var said []string
	for _, k := range keys {
		g := groups[k]
		// the transaction and functional differences point the same way, or there is nothing to carry that is not a rounding artefact
		if g.fn.Sign() == 0 || g.tx.Sign() == 0 || g.fn.Sign() != g.tx.Sign() {
			return nil, exc(FamBal, "DIMENSION_UNBALANCED", "the journal does not balance within %s and the difference cannot be carried (transaction %s, functional %s minor units)",
				label(k), g.tx.String(), g.fn.String()).with("dimension", cfg.Dimension).with("value", k)
		}
		side, info, ref, what := Credit, dueTo, "DUE_TO", "due to the other values of "+cfg.Dimension
		if g.fn.Sign() < 0 {
			side, info, ref, what = Debit, dueFrom, "DUE_FROM", "due from the other values of "+cfg.Dimension
		}
		dims := map[string]string{}
		if k != "" {
			dims[cfg.Dimension] = k
		}
		*lines = append(*lines, &rawLine{t: LineTemplate{LineRef: ref, Side: side, Description: what}, acct: info, dims: dims,
			minor: new(big.Int).Abs(g.tx), funcMn: new(big.Int).Abs(g.fn)})
		said = append(said, label(k)+" "+ref)
	}
	return said, nil
}
