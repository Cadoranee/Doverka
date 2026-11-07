package transfer

import (
	"context"
	"errors"
	"fmt"

	"github.com/Cadoranee/Doverka/internal/clock"
	selection "github.com/Cadoranee/Doverka/internal/provider/select"
	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/rates"
	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

type quoteGetter interface {
	Get(ctx context.Context, quoteID string) (quote.Quote, error)
}

type Runner struct {
	executor  *Executor
	machine   *Machine
	quotes    quoteGetter
	catalog   *refdata.Catalog
	providers []selection.Provider
	rates     *rates.Service
	clk       clock.Clock
}

func NewRunner(executor *Executor, machine *Machine, quotes quoteGetter, catalog *refdata.Catalog, providers []selection.Provider, rateSvc *rates.Service, clk clock.Clock) *Runner {
	return &Runner{
		executor:  executor,
		machine:   machine,
		quotes:    quotes,
		catalog:   catalog,
		providers: providers,
		rates:     rateSvc,
		clk:       clk,
	}
}

func (r *Runner) Run(ctx context.Context, transferID, scenario string) error {
	if r == nil {
		return errors.New("runner is nil")
	}
	if r.executor == nil {
		return errors.New("executor is nil")
	}
	if r.machine == nil {
		return errors.New("machine is nil")
	}
	if r.quotes == nil {
		return errors.New("quotes is nil")
	}
	if r.catalog == nil {
		return errors.New("catalog is nil")
	}
	if r.rates == nil {
		return errors.New("rates is nil")
	}
	if r.clk == nil {
		return errors.New("clock is nil")
	}
	if len(r.providers) == 0 {
		return errors.New("providers are empty")
	}
	if transferID == "" {
		return errors.New("transfer id is empty")
	}

	tr, err := r.machine.Get(ctx, transferID)
	if err != nil {
		return err
	}
	q, err := r.quotes.Get(ctx, tr.QuoteID)
	if err != nil {
		return err
	}
	tariff, ok := r.catalog.Tariff(q.DirectionID)
	if !ok {
		return fmt.Errorf("tariff for direction %q not found", q.DirectionID)
	}

	sourceCur, ok := r.catalog.Currency(q.SourceCurrency)
	if !ok {
		return fmt.Errorf("unknown currency %q", q.SourceCurrency)
	}
	feeCur, ok := r.catalog.Currency(q.FeeCurrency)
	if !ok {
		return fmt.Errorf("unknown currency %q", q.FeeCurrency)
	}
	targetCur, ok := r.catalog.Currency(q.TargetCurrency)
	if !ok {
		return fmt.Errorf("unknown currency %q", q.TargetCurrency)
	}

	minorUnits := map[string]int{
		q.SourceCurrency: sourceCur.MinorUnits,
		q.FeeCurrency:    feeCur.MinorUnits,
		q.TargetCurrency: targetCur.MinorUnits,
	}
	for _, p := range r.providers {
		if p.Currency == "" {
			continue
		}
		if _, known := minorUnits[p.Currency]; known {
			continue
		}
		cur, ok := r.catalog.Currency(p.Currency)
		if !ok {
			return fmt.Errorf("unknown currency %q", p.Currency)
		}
		minorUnits[p.Currency] = cur.MinorUnits
	}

	selReq := selection.Request{
		DirectionID:      q.DirectionID,
		At:               r.clk.Now().UTC(),
		SourceAmount:     minorToDecimal(q.SourceAmountMinor, sourceCur.MinorUnits),
		SourceCurrency:   q.SourceCurrency,
		FeeAmount:        minorToDecimal(q.FeeAmountMinor, feeCur.MinorUnits),
		FeeCurrency:      q.FeeCurrency,
		TargetAmount:     minorToDecimal(q.TargetAmountMinor, targetCur.MinorUnits),
		TargetCurrency:   q.TargetCurrency,
		MinMarginPercent: tariff.MinMarginPercent,
	}

	convert := func(amount decimal.Decimal, from, to string) (decimal.Decimal, error) {
		return r.rates.Convert(ctx, amount, from, to)
	}
	ranked, err := selection.Rank(selReq, r.providers, minorUnits, convert)
	if err != nil {
		return err
	}
	return r.executor.Execute(ctx, ExecuteInput{
		TransferID: transferID,
		Scenario:   scenario,
		MinorUnits: targetCur.MinorUnits,
		Ranked:     ranked,
	})
}

func minorToDecimal(minor int64, units int) decimal.Decimal {
	return decimal.New(minor, -int32(units))
}
