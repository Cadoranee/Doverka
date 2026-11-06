package calc

import (
	"fmt"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

type Charge struct {
	Fee             Money
	AmountToConvert Money
}

func ChargeFee(sourceAmount, rate decimal.Decimal, rule refdata.Fee, sourceCur, targetCur refdata.Currency) (Charge, error) {
	base, feeCur, err := feeBase(sourceAmount, rate, rule.ChargedIn, sourceCur, targetCur)
	if err != nil {
		return Charge{}, err
	}
	raw := base.Mul(rule.Percent).Div(decimal.NewFromInt(100)).Add(rule.Fixed)
	if raw.LessThan(rule.Min) {
		raw = rule.Min
	}
	fee, err := Round(raw, feeCur)
	if err != nil {
		return Charge{}, err
	}
	convert := sourceAmount
	if rule.ChargedIn == "SOURCE" {
		convert = sourceAmount.Sub(fee.Amount)
	}
	amountToConvert, err := Round(convert, sourceCur)
	if err != nil {
		return Charge{}, err
	}
	return Charge{Fee: fee, AmountToConvert: amountToConvert}, nil
}

func feeBase(sourceAmount, rate decimal.Decimal, chargedIn string, sourceCur, targetCur refdata.Currency) (decimal.Decimal, refdata.Currency, error) {
	switch chargedIn {
	case "SOURCE":
		return sourceAmount, sourceCur, nil
	case "TARGET":
		return sourceAmount.Mul(rate), targetCur, nil
	default:
		return decimal.Decimal{}, refdata.Currency{}, fmt.Errorf("invalid charged_in %q", chargedIn)
	}
}
