package calc

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

func TestChargeFee_ex01_RU_TH_BANK(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	tf, ok := cat.Tariff("RU-TH-BANK")
	if !ok {
		t.Fatal("Tariff RU-TH-BANK: want ok")
	}
	got, err := ChargeFee(decimal.RequireFromString("5000"), decimal.RequireFromString("0.42"), tf.Fee, rub, thb)
	if err != nil {
		t.Fatalf("ChargeFee: %v", err)
	}
	assertChargeOK(t, got, rub, rub, "75.00", "4925.00")
}

func TestChargeFee_ex02_min_wins(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	tf, ok := cat.Tariff("RU-TH-BANK")
	if !ok {
		t.Fatal("Tariff RU-TH-BANK: want ok")
	}
	got, err := ChargeFee(decimal.RequireFromString("1100"), decimal.RequireFromString("0.42"), tf.Fee, rub, thb)
	if err != nil {
		t.Fatalf("ChargeFee: %v", err)
	}
	assertChargeOK(t, got, rub, rub, "50.00", "1050.00")
}

func TestChargeFee_ex07_percent_plus_fixed(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	tf, ok := cat.Tariff("RU-TH-CARD")
	if !ok {
		t.Fatal("Tariff RU-TH-CARD: want ok")
	}
	got, err := ChargeFee(decimal.RequireFromString("2500"), decimal.RequireFromString("0.42"), tf.Fee, rub, thb)
	if err != nil {
		t.Fatalf("ChargeFee: %v", err)
	}
	assertChargeOK(t, got, rub, rub, "80.00", "2420.00")
}

func TestChargeFee_ex08_RU_JP(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	jpy := mustCur(t, cat, "JPY")
	tf, ok := cat.Tariff("RU-JP-BANK")
	if !ok {
		t.Fatal("Tariff RU-JP-BANK: want ok")
	}
	got, err := ChargeFee(decimal.RequireFromString("10000"), decimal.RequireFromString("1.9"), tf.Fee, rub, jpy)
	if err != nil {
		t.Fatalf("ChargeFee: %v", err)
	}
	assertChargeOK(t, got, rub, rub, "180.00", "9820.00")
}

func TestChargeFee_ex09_KWD_decimals(t *testing.T) {
	cat := loadFeeCatalog(t)
	kwd := mustCur(t, cat, "KWD")
	thb := mustCur(t, cat, "THB")
	tf, ok := cat.Tariff("KW-TH-BANK")
	if !ok {
		t.Fatal("Tariff KW-TH-BANK: want ok")
	}
	got, err := ChargeFee(decimal.RequireFromString("50"), decimal.RequireFromString("113.75"), tf.Fee, kwd, thb)
	if err != nil {
		t.Fatalf("ChargeFee: %v", err)
	}
	assertChargeOK(t, got, kwd, kwd, "1.000", "49.000")
}

func TestChargeFee_ex10_TARGET_RU_VN(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	vnd := mustCur(t, cat, "VND")
	tf, ok := cat.Tariff("RU-VN-BANK")
	if !ok {
		t.Fatal("Tariff RU-VN-BANK: want ok")
	}
	got, err := ChargeFee(decimal.RequireFromString("7000"), decimal.RequireFromString("300"), tf.Fee, rub, vnd)
	if err != nil {
		t.Fatalf("ChargeFee: %v", err)
	}
	assertChargeOK(t, got, vnd, rub, "30200", "7000.00")
}

func TestChargeFee_half_up(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.RequireFromString("1.25"),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "SOURCE",
	}
	got, err := ChargeFee(decimal.RequireFromString("10"), decimal.RequireFromString("1"), rule, rub, thb)
	if err != nil {
		t.Fatalf("ChargeFee: %v", err)
	}
	assertChargeOK(t, got, rub, rub, "0.13", "9.87")
}

func TestChargeFee_min_then_round(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.Zero,
		Fixed:     decimal.Zero,
		Min:       decimal.RequireFromString("1.006"),
		ChargedIn: "SOURCE",
	}
	got, err := ChargeFee(decimal.RequireFromString("100"), decimal.RequireFromString("1"), rule, rub, thb)
	if err != nil {
		t.Fatalf("ChargeFee: %v", err)
	}
	assertChargeOK(t, got, rub, rub, "1.01", "98.99")
}

func TestChargeFee_err_charged_in(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(1),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "BOTH",
	}
	got, err := ChargeFee(decimal.RequireFromString("100"), decimal.RequireFromString("1"), rule, rub, thb)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "charged_in") {
		t.Fatalf("error %q must contain charged_in", err.Error())
	}
	assertZeroCharge(t, got)
}

func TestChargeFee_err_empty_fee_currency(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := refdata.Currency{Code: "", MinorUnits: 2}
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(1),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "SOURCE",
	}
	got, err := ChargeFee(decimal.RequireFromString("100"), decimal.RequireFromString("1"), rule, rub, thb)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "currency") {
		t.Fatalf("error %q must contain currency", err.Error())
	}
	assertZeroCharge(t, got)
}

func loadFeeCatalog(t *testing.T) *refdata.Catalog {
	t.Helper()
	cat, err := refdata.Load(filepath.Join("..", "..", "data"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cat
}

func mustCur(t *testing.T, cat *refdata.Catalog, code string) refdata.Currency {
	t.Helper()
	c, ok := cat.Currency(code)
	if !ok {
		t.Fatalf("Currency(%q): want ok", code)
	}
	return c
}

func assertMoneyParts(t *testing.T, got Money, cur refdata.Currency, wantAmount string) {
	t.Helper()
	wantDec := decimal.RequireFromString(wantAmount)
	if got.Currency != cur.Code {
		t.Errorf("Currency: got %q want %q", got.Currency, cur.Code)
	}
	if !got.Amount.Equal(wantDec) {
		t.Errorf("Amount: got %s want %s", got.Amount, wantDec)
	}
	wantExp := -int32(cur.MinorUnits)
	if got.Amount.Exponent() != wantExp {
		t.Errorf("Exponent: got %d want %d", got.Amount.Exponent(), wantExp)
	}
}

func assertChargeOK(t *testing.T, c Charge, feeCur, convCur refdata.Currency, wantFee, wantConv string) {
	t.Helper()
	assertMoneyParts(t, c.Fee, feeCur, wantFee)
	assertMoneyParts(t, c.AmountToConvert, convCur, wantConv)
}

func assertZeroCharge(t *testing.T, c Charge) {
	t.Helper()
	if c.Fee.Currency != "" || !c.Fee.Amount.IsZero() {
		t.Errorf("Fee: got %+v want zero", c.Fee)
	}
	if c.AmountToConvert.Currency != "" || !c.AmountToConvert.Amount.IsZero() {
		t.Errorf("AmountToConvert: got %+v want zero", c.AmountToConvert)
	}
}
