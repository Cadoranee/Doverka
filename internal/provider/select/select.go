package selection

import (
	"fmt"
	"sort"
	"time"

	"github.com/Cadoranee/Doverka/internal/calc"
	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

const (
	RecipientAccountInvalid = "RECIPIENT_ACCOUNT_INVALID"
	CurrencyNotSupported    = "CURRENCY_NOT_SUPPORTED"
	AmountOutOfLimits       = "AMOUNT_OUT_OF_LIMITS"
	InternalError           = "INTERNAL_ERROR"
	ProviderDisabled        = "PROVIDER_DISABLED"
	ProviderMaintenance     = "PROVIDER_MAINTENANCE"
)

var (
	weightCost     = decimal.RequireFromString("0.40")
	weightSuccess  = decimal.RequireFromString("0.35")
	weightLatency  = decimal.RequireFromString("0.25")
	limitTolerance = decimal.RequireFromString("0.10")
	percentDivisor = decimal.NewFromInt(100)
)

type ConvertFunc func(amount decimal.Decimal, from, to string) (decimal.Decimal, error)

type Fee struct {
	Percent decimal.Decimal
	Fixed   decimal.Decimal
}

type Limits struct {
	Min decimal.Decimal
	Max decimal.Decimal
}

type Window struct {
	From time.Time
	To   time.Time
}

type Provider struct {
	ID           string
	Directions   []string
	Currency     string
	Fee          Fee
	Limits       Limits
	SuccessRate  decimal.Decimal
	P95LatencyMs int64
	Enabled      bool
	Maintenance  []Window
}

type Request struct {
	DirectionID      string
	At               time.Time
	SourceAmount     decimal.Decimal
	SourceCurrency   string
	FeeAmount        decimal.Decimal
	FeeCurrency      string
	TargetAmount     decimal.Decimal
	TargetCurrency   string
	MinMarginPercent decimal.Decimal
}

type Candidate struct {
	Provider Provider
	Score    decimal.Decimal
	Margin   decimal.Decimal
}

type Ranked struct {
	Primary  []Candidate
	Fallback []Candidate
}

type Attempt struct {
	ProviderID string
	Code       string
}

type priced struct {
	provider Provider
	fee      decimal.Decimal
	margin   decimal.Decimal
}

func AllowsFailover(code string) bool {
	switch code {
	case RecipientAccountInvalid, CurrencyNotSupported, AmountOutOfLimits, InternalError, ProviderDisabled, ProviderMaintenance:
		return true
	default:
		return false
	}
}

func Rank(req Request, providers []Provider, minorUnits map[string]int, convert ConvertFunc) (Ranked, error) {
	if !req.SourceAmount.IsPositive() {
		return Ranked{}, fmt.Errorf("source amount must be positive")
	}
	if req.DirectionID == "" || req.SourceCurrency == "" || req.FeeCurrency == "" || req.TargetCurrency == "" {
		return Ranked{}, fmt.Errorf("direction and currencies are required")
	}
	clientFee, err := inCurrency(req.FeeAmount, req.FeeCurrency, req.SourceCurrency, convert)
	if err != nil {
		return Ranked{}, fmt.Errorf("client fee in source currency: %w", err)
	}
	var primary, fallback []priced
	for _, provider := range providers {
		row, ok, err := priceProvider(req, provider, clientFee, minorUnits, convert)
		if err != nil {
			return Ranked{}, err
		}
		if !ok {
			continue
		}
		if row.margin.LessThan(req.MinMarginPercent) {
			fallback = append(fallback, row)
			continue
		}
		primary = append(primary, row)
	}
	return Ranked{
		Primary:  rankCohort(primary),
		Fallback: rankCohort(fallback),
	}, nil
}

func (r Ranked) Next(attempts []Attempt) (Candidate, bool) {
	if len(r.Primary) == 0 {
		return Candidate{}, false
	}
	if len(attempts) > 0 && !AllowsFailover(attempts[len(attempts)-1].Code) {
		return Candidate{}, false
	}
	seen := make(map[string]struct{}, len(attempts))
	for _, attempt := range attempts {
		seen[attempt.ProviderID] = struct{}{}
	}
	for _, candidate := range r.Primary {
		if _, ok := seen[candidate.Provider.ID]; ok {
			continue
		}
		return candidate, true
	}
	for _, candidate := range r.Fallback {
		if _, ok := seen[candidate.Provider.ID]; ok {
			continue
		}
		return candidate, true
	}
	return Candidate{}, false
}

func priceProvider(req Request, provider Provider, clientFee decimal.Decimal, minorUnits map[string]int, convert ConvertFunc) (priced, bool, error) {
	if !provider.Enabled || provider.P95LatencyMs <= 0 || !serves(provider, req.DirectionID) || inMaintenance(provider, req.At) {
		return priced{}, false, nil
	}
	payout, ok := amountIn(req.TargetAmount, req.TargetCurrency, provider.Currency, convert)
	if !ok {
		return priced{}, false, nil
	}
	if !withinLimits(payout, provider.Limits) {
		return priced{}, false, nil
	}
	raw := payout.Mul(provider.Fee.Percent).Div(percentDivisor).Add(provider.Fee.Fixed)
	units, known := minorUnits[provider.Currency]
	if !known {
		return priced{}, false, fmt.Errorf("minor units for %s are unknown", provider.Currency)
	}
	rounded, err := calc.Round(raw, refdata.Currency{Code: provider.Currency, MinorUnits: units})
	if err != nil {
		return priced{}, false, err
	}
	feeInSource, ok := amountIn(rounded.Amount, provider.Currency, req.SourceCurrency, convert)
	if !ok {
		return priced{}, false, nil
	}
	margin := clientFee.Sub(feeInSource).Div(req.SourceAmount).Mul(percentDivisor)
	return priced{provider: provider, fee: feeInSource, margin: margin}, true, nil
}

func amountIn(amount decimal.Decimal, from, to string, convert ConvertFunc) (decimal.Decimal, bool) {
	if from == to {
		return amount, true
	}
	if convert == nil {
		return decimal.Decimal{}, false
	}
	converted, err := convert(amount, from, to)
	if err != nil {
		return decimal.Decimal{}, false
	}
	return converted, true
}

func inCurrency(amount decimal.Decimal, from, to string, convert ConvertFunc) (decimal.Decimal, error) {
	if from == to {
		return amount, nil
	}
	if convert == nil {
		return decimal.Decimal{}, fmt.Errorf("no rate %s to %s", from, to)
	}
	return convert(amount, from, to)
}

func serves(provider Provider, direction string) bool {
	for _, id := range provider.Directions {
		if id == direction {
			return true
		}
	}
	return false
}

func inMaintenance(provider Provider, at time.Time) bool {
	for _, window := range provider.Maintenance {
		if !at.Before(window.From) && at.Before(window.To) {
			return true
		}
	}
	return false
}

func withinLimits(amount decimal.Decimal, limits Limits) bool {
	one := decimal.NewFromInt(1)
	low := limits.Min.Mul(one.Sub(limitTolerance))
	high := limits.Max.Mul(one.Add(limitTolerance))
	return !amount.LessThan(low) && !amount.GreaterThan(high)
}

func rankCohort(items []priced) []Candidate {
	if len(items) == 0 {
		return nil
	}
	minCost, maxCost := items[0].fee, items[0].fee
	inverses := make([]decimal.Decimal, len(items))
	minInv := inverseLatency(items[0].provider.P95LatencyMs)
	maxInv := minInv
	for i, item := range items {
		inverses[i] = inverseLatency(item.provider.P95LatencyMs)
		if item.fee.LessThan(minCost) {
			minCost = item.fee
		}
		if item.fee.GreaterThan(maxCost) {
			maxCost = item.fee
		}
		if inverses[i].LessThan(minInv) {
			minInv = inverses[i]
		}
		if inverses[i].GreaterThan(maxInv) {
			maxInv = inverses[i]
		}
	}
	out := make([]Candidate, len(items))
	for i, item := range items {
		score := weightCost.Mul(normLowest(item.fee, minCost, maxCost)).
			Add(weightSuccess.Mul(item.provider.SuccessRate)).
			Add(weightLatency.Mul(normHighest(inverses[i], minInv, maxInv)))
		out[i] = Candidate{Provider: item.provider, Score: score, Margin: item.margin}
	}
	sort.SliceStable(out, func(i, j int) bool {
		cmp := out[i].Score.Cmp(out[j].Score)
		if cmp == 0 {
			return out[i].Provider.ID < out[j].Provider.ID
		}
		return cmp > 0
	})
	return out
}

func inverseLatency(ms int64) decimal.Decimal {
	return decimal.NewFromInt(1).Div(decimal.NewFromInt(ms))
}

func normLowest(value, min, max decimal.Decimal) decimal.Decimal {
	if max.Equal(min) {
		return decimal.NewFromInt(1)
	}
	return max.Sub(value).Div(max.Sub(min))
}

func normHighest(value, min, max decimal.Decimal) decimal.Decimal {
	if max.Equal(min) {
		return decimal.NewFromInt(1)
	}
	return value.Sub(min).Div(max.Sub(min))
}
