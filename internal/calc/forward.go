package calc

import (
	"fmt"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

type ForwardQuote struct {
	SourceAmount    Money
	Fee             Money
	AmountToConvert Money
	Rate            decimal.Decimal
	TargetAmount    Money
	Breakdown       []string
}

func Forward(sourceAmount, rate decimal.Decimal, rule refdata.Fee, sourceCur, targetCur refdata.Currency) (ForwardQuote, error) {
	source, err := Round(sourceAmount, sourceCur)
	if err != nil {
		return ForwardQuote{}, err
	}
	charge, err := ChargeFee(source.Amount, rate, rule, sourceCur, targetCur)
	if err != nil {
		return ForwardQuote{}, err
	}
	raw := charge.AmountToConvert.Amount.Mul(rate)
	if rule.ChargedIn == "TARGET" {
		raw = raw.Sub(charge.Fee.Amount)
	}
	target, err := Round(raw, targetCur)
	if err != nil {
		return ForwardQuote{}, err
	}
	return ForwardQuote{
		SourceAmount:    source,
		Fee:             charge.Fee,
		AmountToConvert: charge.AmountToConvert,
		Rate:            rate,
		TargetAmount:    target,
		Breakdown: []string{
			fmt.Sprintf("source_amount %s %s", source.Amount.String(), source.Currency),
			fmt.Sprintf("fee %s %s", charge.Fee.Amount.String(), charge.Fee.Currency),
			fmt.Sprintf("amount_to_convert %s %s", charge.AmountToConvert.Amount.String(), charge.AmountToConvert.Currency),
			fmt.Sprintf("rate %s", rate.String()),
			fmt.Sprintf("target_amount %s %s", target.Amount.String(), target.Currency),
		},
	}, nil
}
