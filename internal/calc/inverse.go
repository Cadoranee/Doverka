package calc

import (
	"fmt"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

func Inverse(targetAmount, rate decimal.Decimal, rule refdata.Fee, sourceCur, targetCur refdata.Currency) (ForwardQuote, error) {
	if !rate.IsPositive() {
		return ForwardQuote{}, fmt.Errorf("rate must be positive")
	}

	quote, err := Forward(decimal.Zero, rate, rule, sourceCur, targetCur)
	if err != nil {
		return ForwardQuote{}, err
	}
	if !quote.TargetAmount.Amount.LessThan(targetAmount) {
		return quote, nil
	}

	step := decimal.New(1, -int32(sourceCur.MinorUnits))
	limit := decimal.New(1, 18)
	lo := decimal.Zero
	hi := step
	for {
		if hi.GreaterThan(limit) {
			hi = limit
		}
		quote, err = Forward(hi, rate, rule, sourceCur, targetCur)
		if err != nil {
			return ForwardQuote{}, err
		}
		if !quote.TargetAmount.Amount.LessThan(targetAmount) {
			break
		}
		if !hi.LessThan(limit) {
			return ForwardQuote{}, fmt.Errorf("target unreachable")
		}
		next := hi.Add(hi)
		if !next.GreaterThan(hi) {
			return ForwardQuote{}, fmt.Errorf("target unreachable")
		}
		lo = hi
		hi = next
	}

	for lo.Add(step).LessThanOrEqual(hi) {
		mid := lo.Add(hi).Div(decimal.NewFromInt(2)).Div(step).Floor().Mul(step)
		if !mid.GreaterThan(lo) {
			break
		}
		probed, ferr := Forward(mid, rate, rule, sourceCur, targetCur)
		if ferr != nil {
			return ForwardQuote{}, ferr
		}
		if !probed.TargetAmount.Amount.LessThan(targetAmount) {
			hi = mid
			quote = probed
			continue
		}
		lo = mid
	}

	return quote, nil
}
