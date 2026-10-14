package ar09

import (
	"context"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func TestValidatePolicy(t *testing.T) {
	p := testPolicy()
	require.NoError(t, ValidatePolicy(&p))

	bad := map[string]func(*Policy){
		"no stages":        func(p *Policy) { p.Stages = nil },
		"gap in stage_no":  func(p *Policy) { p.Stages[1].StageNo = 3 },
		"non increasing":   func(p *Policy) { p.Stages[1].DaysPastDue = 5 },
		"bad channel":      func(p *Policy) { p.Stages[0].Channel = "CARRIER_PIGEON" },
		"no template":      func(p *Policy) { p.Stages[0].TemplateKey = "" },
		"bad weekday":      func(p *Policy) { p.BlackoutWeekdays = []int{7} },
		"bad date":         func(p *Policy) { p.BlackoutDates = []string{"31/12/2026"} },
		"half rate limit":  func(p *Policy) { p.MaxNoticesPerWindow = 2 },
		"negative grace":   func(p *Policy) { p.Stages[0].GraceDays = -1 },
		"no profile":       func(p *Policy) { p.DunningProfile = " " },
		"bad effective":    func(p *Policy) { p.EffectiveFrom = "soon" },
		"negative promise": func(p *Policy) { p.PromiseGraceDays = -1 },
	}
	for name, mut := range bad {
		q := testPolicy()
		mut(&q)
		require.Error(t, ValidatePolicy(&q), name)
	}
}

func TestStateMachine(t *testing.T) {
	require.True(t, CanTransition(StateEligible, StateActive))
	require.True(t, CanTransition(StateActive, StatePromiseToPay))
	require.True(t, CanTransition(StateActive, StateEscalated))
	require.False(t, CanTransition(StateEligible, StateEscalated), "escalation needs dunning to have started")
	require.False(t, CanTransition(StateResolved, StateActive))
	require.False(t, CanTransition(StateClosed, StateActive))
	for from := range transitions {
		require.NotEqual(t, "OVERDUE", from, "overdue is derived from AR-07, never a state")
	}
	for _, s := range []string{StateEligible, StateActive, StatePromiseToPay, StateHeld, StateDisputed, StateEscalated, StateResolved, StateClosed} {
		require.NotEqual(t, "OVERDUE", s)
	}
	require.True(t, IsTerminal(StateResolved) && IsTerminal(StateClosed) && !IsTerminal(StateHeld))
}

func TestAgingDerivedFromViews(t *testing.T) {
	asOf := testNow
	views := []kit.OpenItemView{
		{OpenItemID: "b", Residual: kit.MustAmount("50"), DueDate: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},     // 41 dpd
		{OpenItemID: "a", Residual: kit.MustAmount("10.5"), DueDate: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)}, // 2 dpd
		{OpenItemID: "c", Residual: kit.MustAmount("99"), DueDate: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)},   // current
		{OpenItemID: "z", Residual: 0, DueDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},                        // settled: ignored
	}
	a := BuildAging(views, "USD", asOf)
	require.Equal(t, kit.MustAmount("159.5"), a.Total)
	require.Equal(t, kit.MustAmount("10.5"), a.Buckets["1-30"])
	require.Equal(t, kit.MustAmount("50"), a.Buckets["31-60"])
	require.Equal(t, kit.MustAmount("99"), a.Buckets["CURRENT"])
	require.Len(t, a.Items, 3)
	require.Equal(t, 41, MaxDaysPastDue(a))
	require.False(t, ItemsSettled(views))
	require.True(t, ItemsSettled(views[3:]))
	require.Equal(t, -3, DaysPastDue(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), asOf))
}

func TestNextDueStageAndBlackout(t *testing.T) {
	p := testPolicy()
	p.Stages[1].GraceDays = 3 // trigger 18
	require.Nil(t, p.nextDueStage(0, 4))
	require.Equal(t, 1, p.nextDueStage(0, 5).StageNo)
	require.Equal(t, 2, p.nextDueStage(1, 18).StageNo)
	require.Nil(t, p.nextDueStage(1, 17))
	require.Nil(t, p.nextDueStage(3, 400))

	p.BlackoutWeekdays = []int{1} // Monday (testNow)
	p.BlackoutDates = []string{"2026-12-25"}
	require.Equal(t, SuppContactBlackout, p.BlackoutReason(testNow))
	require.Equal(t, "", p.BlackoutReason(testNow.AddDate(0, 0, 1)))
	require.Equal(t, SuppContactBlackout, p.BlackoutReason(time.Date(2026, 12, 25, 9, 0, 0, 0, time.UTC)))
}

func TestRenderNoticeDeterministic(t *testing.T) {
	cs := &Case{CaseID: "c1", CustomerPartyID: "cust"}
	st := &Stage{StageNo: 1, Channel: "EMAIL", TemplateKey: "DUN1"}
	views := []kit.OpenItemView{{OpenItemID: "i1", Residual: kit.MustAmount("10"), DueDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}}
	a := BuildAging(views, "USD", testNow)
	b1, h1 := RenderNotice(cs, st, a)
	b2, h2 := RenderNotice(cs, st, a)
	require.Equal(t, b1, b2)
	require.Equal(t, h1, h2)
	require.Len(t, h1, 64)
	require.Contains(t, b1, "outstanding=10.0000")
}

func TestAvailableForStateNeverOffersTerminalOrFinancialActions(t *testing.T) {
	require.Empty(t, AvailableForState(StateResolved))
	require.Empty(t, AvailableForState(StateClosed))
	require.Contains(t, AvailableForState(StateHeld), "ResumeCollections")
	require.NotContains(t, AvailableForState(StateActive), "ResumeCollections")
	for _, st := range []string{StateEligible, StateActive, StatePromiseToPay, StateHeld, StateDisputed, StateEscalated} {
		for _, a := range AvailableForState(st) {
			require.Contains(t, ActionFor, a)
		}
	}
}

// TestNeg29_CollectorHasNoWriteOffAuthority (§18 #29 / AR-09 negative #3): collections has no approval, write-off, refund,
// credit or settlement path. Proven three ways: no route, no authz action, and the source has no balance-mutating call.
func TestNeg29_CollectorHasNoWriteOffAuthority(t *testing.T) {
	m := New(testDeps(&fakeAuthz{}, nil), newFakeRecv(), &fakeProfiles{}, nil)
	r := chi.NewRouter()
	m.Register(r)
	forbidden := regexp.MustCompile(`(?i)write.?off|refund|credit|approv|settle|apply.?cash|adjust|payment|reverse|post`)
	n := 0
	require.NoError(t, chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		n++
		require.False(t, forbidden.MatchString(route), "route %s %s must not exist in collections", method, route)
		return nil
	}))
	require.Equal(t, 5*2+9, n, "5 queries (GET+POST) and 9 commands")
	for _, a := range Actions {
		require.False(t, forbidden.MatchString(a), a)
	}
	for _, c := range []string{"ApproveWriteOff", "RequestWriteOff", "WriteOff", "ApplyReceivableAdjustment", "ApplyCash"} {
		_, ok := ActionFor[c]
		require.False(t, ok, c)
	}
}

// TestNoBalanceMutationInSource: no non-test source of this package calls a balance-mutating or posting API, touches an AR-07
// table, or writes the posting queue. Collections can only read AR-07 through the Receivables interface.
func TestNoBalanceMutationInSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	banned := []string{"ApplyAdjustment", "CreateFromDocument", "QueuePosting", "kit.OpenItems", "kit.PostingEvent", "AdjWriteOff", "AdjCashApplication",
		"AdjRefund", "AdjCreditNote", "AdjCorrection", "ar_accounting_posting_requests", "ar07_", "residual_amount", "kit.Adj", "ar07.", "ar10", "ar08"}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		src := string(raw)
		for _, b := range banned {
			// comments may name them; strip comment lines before scanning
			code := stripComments(src)
			require.False(t, strings.Contains(code, b), "%s must not reference %s", f, b)
		}
		_, err = parser.ParseFile(token.NewFileSet(), f, raw, parser.ImportsOnly)
		require.NoError(t, err)
	}
	// Receivables has exactly the three read methods.
	require.Equal(t, 3, interfaceMethodCount(t, "module.go", "Receivables"))
}

func stripComments(src string) string {
	var out []string
	for _, l := range strings.Split(src, "\n") {
		if i := strings.Index(l, "//"); i >= 0 && !strings.Contains(l[:i], "`") && !strings.Contains(l[:i], "\"") {
			l = l[:i]
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func interfaceMethodCount(t *testing.T, file, name string) int {
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	src := string(raw)
	i := strings.Index(src, "type "+name+" interface {")
	require.GreaterOrEqual(t, i, 0)
	body := src[i:]
	body = body[:strings.Index(body, "\n}")]
	return strings.Count(body, "ctx context.Context")
}

// A delivery receipt is communication evidence: the Receipt type carries no amount and nothing derives a balance from it.
func TestDeliveryReceiptCarriesNoMoney(t *testing.T) {
	require.Equal(t, 2, reflectFieldCount(Receipt{}))
}

func TestUnconfiguredDelivererNeverFakesSuccess(t *testing.T) {
	_, err := UnconfiguredDeliverer{}.Deliver(context.Background(), Notice{})
	require.ErrorIs(t, err, ErrDeliveryNotConfigured)
}

func reflectFieldCount(v any) int { return reflect.TypeOf(v).NumField() }
