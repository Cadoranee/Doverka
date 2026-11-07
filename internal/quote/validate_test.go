package quote

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

func loadCatalog(t *testing.T) *refdata.Catalog {
	t.Helper()
	cat, err := refdata.Load(filepath.Join("..", "..", "data"))
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	return cat
}

func ruThBankInput(amount, amountType string) Input {
	return Input{
		SourceCountry:  "RU",
		SourceCurrency: "RUB",
		TargetCountry:  "TH",
		TargetCurrency: "THB",
		PayoutMethod:   "BANK_ACCOUNT",
		Amount:         amount,
		AmountType:     amountType,
	}
}

func assertCode(t *testing.T, err error, want Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error code %s, got nil", want)
	}
	if !errors.Is(err, &Error{Code: want}) {
		t.Fatalf("want errors.Is %s, got %v", want, err)
	}
}

func TestResolve_example04_below_min(t *testing.T) {
	cat := loadCatalog(t)
	_, err := Resolve(cat, ruThBankInput("500.00", "SOURCE"))
	assertCode(t, err, CodeAmountBelowMin)
}

func TestResolve_example05_above_max(t *testing.T) {
	cat := loadCatalog(t)
	_, err := Resolve(cat, ruThBankInput("600000.00", "SOURCE"))
	assertCode(t, err, CodeAmountAboveMax)
}

func TestResolve_example06_unsupported_method(t *testing.T) {
	cat := loadCatalog(t)
	in := ruThBankInput("2000.00", "SOURCE")
	in.PayoutMethod = "CASH_PICKUP"
	_, err := Resolve(cat, in)
	assertCode(t, err, CodeUnsupportedDirection)
}

func TestResolve_inclusive_limits(t *testing.T) {
	cat := loadCatalog(t)
	for _, amount := range []string{"1000.00", "300000.00"} {
		got, err := Resolve(cat, ruThBankInput(amount, "SOURCE"))
		if err != nil {
			t.Fatalf("amount %s: %v", amount, err)
		}
		if got.Tariff.DirectionID != "RU-TH-BANK" {
			t.Fatalf("amount %s: DirectionID %q", amount, got.Tariff.DirectionID)
		}
		wantAmt := decimal.RequireFromString(amount)
		if !got.Amount.Equal(wantAmt) {
			t.Fatalf("amount %s: got %s want %s", amount, got.Amount, wantAmt)
		}
		if got.AmountCurrency != "RUB" {
			t.Fatalf("amount %s: AmountCurrency %q", amount, got.AmountCurrency)
		}
	}
}

func TestResolve_rub_amount_format(t *testing.T) {
	cat := loadCatalog(t)
	bad := []string{"2000", "2000.0", "2000.000", "abc", "", "-10.00", "0.00", " 1000.00"}
	for _, amount := range bad {
		in := ruThBankInput(amount, "SOURCE")
		if amount == "" {
			in.Amount = ""
		}
		_, err := Resolve(cat, in)
		assertCode(t, err, CodeValidationError)
	}
}

func TestResolve_bad_amount_type(t *testing.T) {
	cat := loadCatalog(t)
	_, err := Resolve(cat, ruThBankInput("1000.00", "GROSS"))
	assertCode(t, err, CodeValidationError)
}

func TestResolve_empty_source_country(t *testing.T) {
	cat := loadCatalog(t)
	in := ruThBankInput("1000.00", "SOURCE")
	in.SourceCountry = ""
	_, err := Resolve(cat, in)
	assertCode(t, err, CodeValidationError)
}

func TestResolve_jpy_target_amount(t *testing.T) {
	cat := loadCatalog(t)
	base := Input{
		SourceCountry:  "RU",
		SourceCurrency: "RUB",
		TargetCountry:  "JP",
		TargetCurrency: "JPY",
		PayoutMethod:   "BANK_ACCOUNT",
		AmountType:     "TARGET",
	}
	base.Amount = "50000"
	got, err := Resolve(cat, base)
	if err != nil {
		t.Fatalf("50000 TARGET: %v", err)
	}
	if got.AmountCurrency != "JPY" {
		t.Fatalf("AmountCurrency %q want JPY", got.AmountCurrency)
	}
	base.Amount = "50000.00"
	_, err = Resolve(cat, base)
	assertCode(t, err, CodeValidationError)
}

func TestResolve_target_skips_source_limits(t *testing.T) {
	cat := loadCatalog(t)
	got, err := Resolve(cat, ruThBankInput("1.00", "TARGET"))
	if err != nil {
		t.Fatalf("TARGET 1.00: %v", err)
	}
	if got.AmountCurrency != "THB" {
		t.Fatalf("AmountCurrency %q want THB", got.AmountCurrency)
	}
}

func TestResolve_kwd_source_format(t *testing.T) {
	cat := loadCatalog(t)
	base := Input{
		SourceCountry:  "KW",
		SourceCurrency: "KWD",
		TargetCountry:  "TH",
		TargetCurrency: "THB",
		PayoutMethod:   "BANK_ACCOUNT",
		AmountType:     "SOURCE",
	}
	base.Amount = "20.000"
	if _, err := Resolve(cat, base); err != nil {
		t.Fatalf("20.000: %v", err)
	}
	base.Amount = "20.00"
	_, err := Resolve(cat, base)
	assertCode(t, err, CodeValidationError)
	base.Amount = "19.999"
	_, err = Resolve(cat, base)
	assertCode(t, err, CodeAmountBelowMin)
	base.Amount = "5000.000"
	if _, err := Resolve(cat, base); err != nil {
		t.Fatalf("5000.000: %v", err)
	}
}

func TestCheckSourceLimits_ru_th_bank(t *testing.T) {
	cat := loadCatalog(t)
	tf, ok := cat.Tariff("RU-TH-BANK")
	if !ok {
		t.Fatal("RU-TH-BANK tariff missing")
	}
	cases := []struct {
		amount string
		code   Code
		ok     bool
	}{
		{"999.99", CodeAmountBelowMin, false},
		{"1000", "", true},
		{"300000", "", true},
		{"300000.01", CodeAmountAboveMax, false},
	}
	for _, tt := range cases {
		err := CheckSourceLimits(tf, decimal.RequireFromString(tt.amount))
		if tt.ok {
			if err != nil {
				t.Errorf("amount %s: %v", tt.amount, err)
			}
			continue
		}
		assertCode(t, err, tt.code)
	}
}

func TestResolve_unknown_route(t *testing.T) {
	cat := loadCatalog(t)
	in := Input{
		SourceCountry:  "ZZ",
		SourceCurrency: "XXX",
		TargetCountry:  "TH",
		TargetCurrency: "THB",
		PayoutMethod:   "BANK_ACCOUNT",
		Amount:         "1000.00",
		AmountType:     "SOURCE",
	}
	_, err := Resolve(cat, in)
	assertCode(t, err, CodeUnsupportedDirection)
}

func TestResolve_unsupported_before_amount_format(t *testing.T) {
	cat := loadCatalog(t)
	in := ruThBankInput("abc", "SOURCE")
	in.PayoutMethod = "CASH_PICKUP"
	_, err := Resolve(cat, in)
	assertCode(t, err, CodeUnsupportedDirection)
}

func TestResolve_nil_catalog(t *testing.T) {
	_, err := Resolve(nil, ruThBankInput("1000.00", "SOURCE"))
	assertCode(t, err, CodeUnsupportedDirection)
}
