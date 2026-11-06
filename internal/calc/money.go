package calc

import (
	"fmt"
	"math"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

type Money struct {
	Amount   decimal.Decimal
	Currency string
}

func Round(v decimal.Decimal, currency refdata.Currency) (Money, error) {
	if currency.Code == "" {
		return Money{}, fmt.Errorf("currency code is empty")
	}
	if currency.MinorUnits < 0 || currency.MinorUnits > math.MaxInt32 {
		return Money{}, fmt.Errorf("minor_units out of range")
	}
	rounded := v.Round(int32(currency.MinorUnits))
	return Money{
		Amount:   rounded,
		Currency: currency.Code,
	}, nil
}
