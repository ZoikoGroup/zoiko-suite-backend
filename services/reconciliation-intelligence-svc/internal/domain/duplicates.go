package domain

import (
	"math/big"
	"regexp"
	"sort"
	"strings"
)

// Duplicate-entry detection within one bank-reconciliation run's UNMATCHED
// population: statement lines that are probably the same bank entry
// recorded twice (a statement imported twice, a line re-keyed with a
// sequence suffix).
//
// Two lines are probable duplicates when ALL of these hold:
//
//  1. Same bank account and statement date. Not checked here because the
//     population guarantees it: a run is exactly one (bank_account_id,
//     statement_date), and internal/bankrec refuses any line outside it. So
//     "same or close date" is "same date" by construction; an entry
//     duplicated across two statement dates is in two runs and is not seen.
//  2. Exactly the same amount, sign included, compared as big.Rat. A +100
//     and a -100 are a payment and its reversal, not a duplicate.
//  3. The same currency_code.
//  4. Matching bank references, where a reference is first normalized by
//     upper-casing it and removing all whitespace, and then either:
//     a. the normalized references are equal ("REF 123" / "ref123"), or
//     b. one is the other plus ONE trailing sequence suffix: a separator
//     (- _ / . #) followed by 1-2 digits, or 1-2 digits in parentheses
//     ("REF123" / "REF123-1", "REF123" / "REF123 (2)").
//     Two DIFFERENT suffixes on a common stem ("PAY-1" / "PAY-2") are not
//     a match: that is how distinct recurring payments are numbered. An
//     empty reference never matches anything — no evidence either way.
//
// Within a group of mutual duplicates one line is the anchor — the entry
// that stays — and every other line is reported as a duplicate OF it. The
// anchor is an unsuffixed original when the group has one, otherwise the
// lowest statement_line_id (bank-reconciliation-svc gives no ingestion
// order to prefer).

var duplicateSuffix = regexp.MustCompile(`(?:[-_/.#][0-9]{1,2}|\([0-9]{1,2}\))$`)

func normalizeReference(ref string) string {
	return strings.ToUpper(strings.Join(strings.Fields(ref), ""))
}

// duplicateOf records that a line is a probable duplicate of anchor. exact
// is true when the normalized references are equal (rule 4a) rather than
// related by a suffix (4b).
type duplicateOf struct {
	anchor int
	exact  bool
}

// findDuplicates returns, by line index, the lines that are probable
// duplicates of another line, and for each anchor the indexes of its
// duplicates. amounts[i] is lines[i]'s parsed amount.
func findDuplicates(lines []BankStatementLine, amounts []*big.Rat) (map[int]duplicateOf, map[int][]int) {
	norm := make([]string, len(lines))
	groups := map[string][]int{}
	for i, l := range lines {
		norm[i] = normalizeReference(l.BankReference)
		base := duplicateSuffix.ReplaceAllString(norm[i], "")
		if norm[i] == "" || base == "" {
			continue
		}
		key := CanonicalAmount(amounts[i]) + "\x00" + l.CurrencyCode + "\x00" + base
		groups[key] = append(groups[key], i)
	}

	dupOf := map[int]duplicateOf{}
	dupsOf := map[int][]int{}
	link := func(anchor int, members []int) {
		for _, m := range members {
			if m == anchor {
				continue
			}
			dupOf[m] = duplicateOf{anchor: anchor, exact: norm[m] == norm[anchor]}
			dupsOf[anchor] = append(dupsOf[anchor], m)
		}
	}

	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(a, b int) bool {
			return lines[members[a]].StatementLineID < lines[members[b]].StatementLineID
		})
		base := duplicateSuffix.ReplaceAllString(norm[members[0]], "")

		// An unsuffixed original: every member is either an exact copy of
		// it or it plus one suffix (rule 4a or 4b).
		anchor := -1
		for _, m := range members {
			if norm[m] == base {
				anchor = m
				break
			}
		}
		if anchor >= 0 {
			link(anchor, members)
			continue
		}

		// No original, only suffixed references: only identical ones are
		// duplicates of each other (rule 4a). "PAY-1" and "PAY-2" stay apart.
		byNorm := map[string][]int{}
		var order []string
		for _, m := range members {
			if _, seen := byNorm[norm[m]]; !seen {
				order = append(order, norm[m])
			}
			byNorm[norm[m]] = append(byNorm[norm[m]], m)
		}
		for _, n := range order {
			if same := byNorm[n]; len(same) > 1 {
				link(same[0], same)
			}
		}
	}

	for a := range dupsOf {
		sort.Ints(dupsOf[a])
	}
	return dupOf, dupsOf
}
