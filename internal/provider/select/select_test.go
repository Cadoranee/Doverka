package selection

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestRank_primaryFiltersAndScore(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	req := baseRequest(at)
	lowCost := baseProvider("low")
	lowCost.Fee.Fixed = decimal.NewFromInt(1)
	lowCost.SuccessRate = decimal.RequireFromString("0.50")
	highCost := baseProvider("high")
	highCost.Fee.Fixed = decimal.NewFromInt(3)
	highCost.SuccessRate = decimal.RequireFromString("0.90")
	disabled := baseProvider("disabled")
	disabled.Enabled = false
	other := baseProvider("other")
	other.Directions = []string{"RU-JP-BANK"}
	maint := baseProvider("maint")
	maint.Maintenance = []Window{{
		From: at,
		To:   at.Add(30 * time.Minute),
	}}

	ranked, err := Rank(req, []Provider{highCost, disabled, other, maint, lowCost}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if ids(ranked.Primary) != "low,high" || len(ranked.Fallback) != 0 {
		t.Fatalf("primary=%s fallback=%s", ids(ranked.Primary), ids(ranked.Fallback))
	}
	if !ranked.Primary[0].Score.Equal(decimal.RequireFromString("0.825")) {
		t.Fatalf("low score=%s", ranked.Primary[0].Score)
	}
	if !ranked.Primary[1].Score.Equal(decimal.RequireFromString("0.565")) {
		t.Fatalf("high score=%s", ranked.Primary[1].Score)
	}
}

func TestRank_lowerLatencyScoresHigherWhenCostMatches(t *testing.T) {
	req := baseRequest(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	slow := baseProvider("slow")
	slow.P95LatencyMs = 200
	fast := baseProvider("fast")
	fast.P95LatencyMs = 100
	ranked, err := Rank(req, []Provider{slow, fast}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if ids(ranked.Primary) != "fast,slow" {
		t.Fatalf("primary=%s", ids(ranked.Primary))
	}
	if !ranked.Primary[0].Score.Equal(decimal.RequireFromString("0.825")) {
		t.Fatalf("fast score=%s", ranked.Primary[0].Score)
	}
	if !ranked.Primary[1].Score.Equal(decimal.RequireFromString("0.575")) {
		t.Fatalf("slow score=%s", ranked.Primary[1].Score)
	}
}

func TestRank_maintenanceEndIsExclusive(t *testing.T) {
	start := time.Date(2026, 11, 7, 22, 0, 0, 0, time.UTC)
	provider := baseProvider("alpha")
	provider.Maintenance = []Window{{From: start, To: start.Add(30 * time.Minute)}}
	during, err := Rank(baseRequest(start), []Provider{provider}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("during: %v", err)
	}
	if len(during.Primary) != 0 {
		t.Fatal("window start must exclude the provider")
	}
	after, err := Rank(baseRequest(start.Add(30*time.Minute)), []Provider{provider}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if ids(after.Primary) != "alpha" {
		t.Fatalf("primary=%s", ids(after.Primary))
	}
}

func TestRank_limitsUseTenPercentBand(t *testing.T) {
	provider := baseProvider("band")
	provider.Limits = Limits{Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(100)}
	provider.Fee.Fixed = decimal.Zero
	cases := []struct {
		amount string
		want   bool
	}{
		{"9", true},
		{"8.99", false},
		{"110", true},
		{"110.01", false},
	}
	for _, tc := range cases {
		req := baseRequest(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
		req.TargetAmount = decimal.RequireFromString(tc.amount)
		ranked, err := Rank(req, []Provider{provider}, usdMinors(), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.amount, err)
		}
		got := ids(ranked.Primary) == "band"
		if got != tc.want {
			t.Fatalf("amount %s included=%v", tc.amount, got)
		}
	}
}

func TestRank_limitsAndFeeUseConvertedPayout(t *testing.T) {
	provider := baseProvider("fx")
	provider.Currency = "USD"
	provider.Limits = Limits{Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(100)}
	provider.Fee = Fee{Percent: decimal.Zero, Fixed: decimal.Zero}
	req := baseRequest(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	req.TargetAmount = decimal.NewFromInt(1000)
	req.TargetCurrency = "THB"
	inside := func(amount decimal.Decimal, from, to string) (decimal.Decimal, error) {
		return decimal.NewFromInt(30), nil
	}
	ranked, err := Rank(req, []Provider{provider}, usdMinors(), inside)
	if err != nil {
		t.Fatalf("inside: %v", err)
	}
	if ids(ranked.Primary) != "fx" {
		t.Fatalf("primary=%s", ids(ranked.Primary))
	}
	outside := func(amount decimal.Decimal, from, to string) (decimal.Decimal, error) {
		return decimal.NewFromInt(500), nil
	}
	ranked, err = Rank(req, []Provider{provider}, usdMinors(), outside)
	if err != nil {
		t.Fatalf("outside: %v", err)
	}
	if len(ranked.Primary) != 0 || len(ranked.Fallback) != 0 {
		t.Fatalf("primary=%s fallback=%s", ids(ranked.Primary), ids(ranked.Fallback))
	}
}

func TestRank_marginSplitsPrimaryAndFallback(t *testing.T) {
	req := baseRequest(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	req.FeeAmount = decimal.NewFromInt(10)
	req.MinMarginPercent = decimal.NewFromInt(5)
	okFee := baseProvider("ok")
	okFee.Fee.Fixed = decimal.NewFromInt(1)
	low := baseProvider("low")
	low.Fee.Fixed = decimal.NewFromInt(50)
	ranked, err := Rank(req, []Provider{low, okFee}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if ids(ranked.Primary) != "ok" || ids(ranked.Fallback) != "low" {
		t.Fatalf("primary=%s fallback=%s", ids(ranked.Primary), ids(ranked.Fallback))
	}
}

func TestRank_providerFeeRoundsHalfUpBeforeMargin(t *testing.T) {
	req := baseRequest(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	req.SourceAmount = decimal.NewFromInt(1)
	req.FeeAmount = decimal.RequireFromString("0.015")
	req.TargetAmount = decimal.NewFromInt(1)
	req.MinMarginPercent = decimal.RequireFromString("0.75")
	provider := baseProvider("round")
	provider.Fee.Fixed = decimal.RequireFromString("0.005")
	ranked, err := Rank(req, []Provider{provider}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(ranked.Primary) != 0 || ids(ranked.Fallback) != "round" {
		t.Fatalf("primary=%s fallback=%s", ids(ranked.Primary), ids(ranked.Fallback))
	}
	if !ranked.Fallback[0].Margin.Equal(decimal.RequireFromString("0.5")) {
		t.Fatalf("margin=%s", ranked.Fallback[0].Margin)
	}
}

func TestNext_recipientAccountInvalidTriesNextThenFallback(t *testing.T) {
	req := baseRequest(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	req.MinMarginPercent = decimal.NewFromInt(5)
	first := baseProvider("first")
	first.Fee.Fixed = decimal.NewFromInt(1)
	second := baseProvider("second")
	second.Fee.Fixed = decimal.NewFromInt(2)
	below := baseProvider("below")
	below.Fee.Fixed = decimal.NewFromInt(50)
	ranked, err := Rank(req, []Provider{below, second, first}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	got, ok := ranked.Next(nil)
	if !ok || got.Provider.ID != "first" {
		t.Fatalf("first=%s ok=%v", got.Provider.ID, ok)
	}
	got, ok = ranked.Next([]Attempt{{ProviderID: "first", Code: RecipientAccountInvalid}})
	if !ok || got.Provider.ID != "second" {
		t.Fatalf("second=%s ok=%v", got.Provider.ID, ok)
	}
	got, ok = ranked.Next([]Attempt{
		{ProviderID: "first", Code: RecipientAccountInvalid},
		{ProviderID: "second", Code: InternalError},
	})
	if !ok || got.Provider.ID != "below" {
		t.Fatalf("fallback=%s ok=%v", got.Provider.ID, ok)
	}
}

func TestNext_unknownDoesNotSwitchProvider(t *testing.T) {
	req := baseRequest(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	first := baseProvider("first")
	first.Fee.Fixed = decimal.NewFromInt(1)
	second := baseProvider("second")
	second.Fee.Fixed = decimal.NewFromInt(2)
	ranked, err := Rank(req, []Provider{first, second}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if _, ok := ranked.Next([]Attempt{{ProviderID: "first", Code: "UNKNOWN"}}); ok {
		t.Fatal("unknown outcome must not select another provider")
	}
}

func TestNext_withoutPrimaryDoesNotStartFromFallback(t *testing.T) {
	req := baseRequest(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	req.MinMarginPercent = decimal.NewFromInt(50)
	provider := baseProvider("below")
	provider.Fee.Fixed = decimal.NewFromInt(40)
	ranked, err := Rank(req, []Provider{provider}, usdMinors(), nil)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(ranked.Primary) != 0 || ids(ranked.Fallback) != "below" {
		t.Fatalf("primary=%s fallback=%s", ids(ranked.Primary), ids(ranked.Fallback))
	}
	if _, ok := ranked.Next(nil); ok {
		t.Fatal("fallback must not be the first attempt")
	}
}

func baseRequest(at time.Time) Request {
	return Request{
		DirectionID:      "RU-TH-BANK",
		At:               at,
		SourceAmount:     decimal.NewFromInt(100),
		SourceCurrency:   "USD",
		FeeAmount:        decimal.NewFromInt(10),
		FeeCurrency:      "USD",
		TargetAmount:     decimal.NewFromInt(100),
		TargetCurrency:   "USD",
		MinMarginPercent: decimal.Zero,
	}
}

func baseProvider(id string) Provider {
	return Provider{
		ID:           id,
		Directions:   []string{"RU-TH-BANK"},
		Currency:     "USD",
		Fee:          Fee{Percent: decimal.Zero, Fixed: decimal.NewFromInt(1)},
		Limits:       Limits{Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(100000)},
		SuccessRate:  decimal.RequireFromString("0.50"),
		P95LatencyMs: 100,
		Enabled:      true,
	}
}

func usdMinors() map[string]int {
	return map[string]int{"USD": 2}
}

func ids(candidates []Candidate) string {
	if len(candidates) == 0 {
		return ""
	}
	out := candidates[0].Provider.ID
	for _, candidate := range candidates[1:] {
		out += "," + candidate.Provider.ID
	}
	return out
}
