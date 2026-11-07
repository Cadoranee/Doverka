package quote

import (
	"strings"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

type Code string

const (
	CodeValidationError      Code = "VALIDATION_ERROR"
	CodeUnsupportedDirection Code = "UNSUPPORTED_DIRECTION"
	CodeAmountBelowMin       Code = "AMOUNT_BELOW_MIN"
	CodeAmountAboveMax       Code = "AMOUNT_ABOVE_MAX"
)

type Error struct {
	Code Code
}

func (e *Error) Error() string {
	return string(e.Code)
}

func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	te, ok := target.(*Error)
	if !ok || te == nil {
		return false
	}
	return e.Code == te.Code
}

type Input struct {
	SourceCountry  string
	SourceCurrency string
	TargetCountry  string
	TargetCurrency string
	PayoutMethod   string
	Amount         string
	AmountType     string
}

type Resolved struct {
	Tariff         refdata.Tariff
	Amount         decimal.Decimal
	AmountCurrency string
}

func Resolve(cat *refdata.Catalog, in Input) (Resolved, error) {
	if in.SourceCountry == "" ||
		in.SourceCurrency == "" ||
		in.TargetCountry == "" ||
		in.TargetCurrency == "" ||
		in.PayoutMethod == "" ||
		in.Amount == "" {
		return Resolved{}, &Error{Code: CodeValidationError}
	}
	if in.AmountType != "SOURCE" && in.AmountType != "TARGET" {
		return Resolved{}, &Error{Code: CodeValidationError}
	}

	tariff, ok := cat.FindTariff(in.SourceCountry, in.SourceCurrency, in.TargetCountry, in.TargetCurrency, in.PayoutMethod)
	if !ok {
		return Resolved{}, &Error{Code: CodeUnsupportedDirection}
	}

	amountCurrency := tariff.Source.Currency
	if in.AmountType == "TARGET" {
		amountCurrency = tariff.Target.Currency
	}

	cur, ok := cat.Currency(amountCurrency)
	if !ok {
		return Resolved{}, &Error{Code: CodeValidationError}
	}

	if !amountIsValid(in.Amount, cur.MinorUnits) {
		return Resolved{}, &Error{Code: CodeValidationError}
	}

	amount, err := decimal.NewFromString(in.Amount)
	if err != nil || !amount.GreaterThan(decimal.Zero) {
		return Resolved{}, &Error{Code: CodeValidationError}
	}

	if in.AmountType == "SOURCE" {
		if err := CheckSourceLimits(tariff, amount); err != nil {
			return Resolved{}, err
		}
	}

	return Resolved{
		Tariff:         tariff,
		Amount:         amount,
		AmountCurrency: amountCurrency,
	}, nil
}

func CheckSourceLimits(tariff refdata.Tariff, sourceAmount decimal.Decimal) error {
	if sourceAmount.Cmp(tariff.AmountLimits.Min) < 0 {
		return &Error{Code: CodeAmountBelowMin}
	}
	if sourceAmount.Cmp(tariff.AmountLimits.Max) > 0 {
		return &Error{Code: CodeAmountAboveMax}
	}
	return nil
}

func amountIsValid(text string, minor int) bool {
	if minor < 0 {
		return false
	}
	whole, fraction, hasDot := strings.Cut(text, ".")
	if !asciiDigits(whole) {
		return false
	}
	if minor == 0 {
		return !hasDot
	}
	if !hasDot || strings.Contains(fraction, ".") {
		return false
	}
	return len(fraction) == minor && asciiDigits(fraction)
}

func asciiDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
