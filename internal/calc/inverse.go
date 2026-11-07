package calc

import (
	"errors"
	"fmt"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

const (
	maxCorrectionSteps = 10000
	divisionScale      = 28
)

var errTargetUnreachable = errors.New("target unreachable")

func Inverse(targetAmount, rate decimal.Decimal, rule refdata.Fee, sourceCur, targetCur refdata.Currency) (ForwardQuote, error) {
	if !rate.IsPositive() {
		return ForwardQuote{}, fmt.Errorf("rate must be positive")
	}
	if _, err := Forward(decimal.Zero, rate, rule, sourceCur, targetCur); err != nil {
		return ForwardQuote{}, err
	}
	estimate, err := estimateSource(targetAmount, rate, rule)
	if err != nil {
		return ForwardQuote{}, err
	}
	rounded, err := Round(estimate, sourceCur)
	if err != nil {
		return ForwardQuote{}, err
	}

	step := decimal.New(1, -int32(sourceCur.MinorUnits))
	source := decimal.Max(rounded.Amount, step)
	quote, err := Forward(source, rate, rule, sourceCur, targetCur)
	if err != nil {
		return ForwardQuote{}, err
	}

	for steps := 0; quote.TargetAmount.Amount.LessThan(targetAmount); steps++ {
		if steps == maxCorrectionSteps {
			return ForwardQuote{}, errTargetUnreachable
		}
		source = source.Add(step)
		if quote, err = Forward(source, rate, rule, sourceCur, targetCur); err != nil {
			return ForwardQuote{}, err
		}
	}

	for steps := 0; steps < maxCorrectionSteps && quote.TargetAmount.Amount.GreaterThan(targetAmount) && source.GreaterThan(step); steps++ {
		lower, err := Forward(source.Sub(step), rate, rule, sourceCur, targetCur)
		if err != nil {
			return ForwardQuote{}, err
		}
		if lower.TargetAmount.Amount.LessThan(targetAmount) {
			break
		}
		source, quote = source.Sub(step), lower
	}

	return quote, nil
}

func estimateSource(target, rate decimal.Decimal, rule refdata.Fee) (decimal.Decimal, error) {
	share := rule.Percent.Div(decimal.NewFromInt(100))
	if share.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		return decimal.Decimal{}, errTargetUnreachable
	}
	switch rule.ChargedIn {
	case "SOURCE":
		return grossBeforeFee(target.DivRound(rate, divisionScale), share, rule), nil
	case "TARGET":
		return grossBeforeFee(target, share, rule).DivRound(rate, divisionScale), nil
	default:
		return decimal.Decimal{}, fmt.Errorf("invalid charged_in %q", rule.ChargedIn)
	}
}

func grossBeforeFee(net, share decimal.Decimal, rule refdata.Fee) decimal.Decimal {
	withMinFee := net.Add(rule.Min)
	if withMinFee.Mul(share).Add(rule.Fixed).LessThanOrEqual(rule.Min) {
		return withMinFee
	}
	return net.Add(rule.Fixed).DivRound(decimal.NewFromInt(1).Sub(share), divisionScale)
}
