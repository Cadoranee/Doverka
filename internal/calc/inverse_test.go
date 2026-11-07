package calc

import (
	"strings"
	"testing"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

func TestInverse_ex03_RU_TH_BANK(t *testing.T) {
	cat := loadFeeCatalog(t)
	tf, ok := cat.Tariff("RU-TH-BANK")
	if !ok {
		t.Fatal("Tariff RU-TH-BANK: want ok")
	}
	src := mustCur(t, cat, "RUB")
	feeCur := mustCur(t, cat, "RUB")
	tgt := mustCur(t, cat, "THB")
	rate := decimal.RequireFromString("0.42")
	target := decimal.RequireFromString("124068.63")

	got, err := Inverse(target, rate, tf.Fee, src, tgt)
	if err != nil {
		t.Fatalf("Inverse: %v", err)
	}
	assertForwardOK(t, got, rate, src, feeCur, src, tgt, "299899.99", "4498.50", "295401.49", "124068.63")

	f98, err := Forward(decimal.RequireFromString("299899.98"), rate, tf.Fee, src, tgt)
	if err != nil {
		t.Fatalf("Forward(299899.98): %v", err)
	}
	assertMoneyParts(t, f98.TargetAmount, tgt, "124068.62")

	f99, err := Forward(decimal.RequireFromString("299899.99"), rate, tf.Fee, src, tgt)
	if err != nil {
		t.Fatalf("Forward(299899.99): %v", err)
	}
	assertMoneyParts(t, f99.TargetAmount, tgt, "124068.63")
}

func TestInverse_err_rate(t *testing.T) {
	cat := loadFeeCatalog(t)
	tf, ok := cat.Tariff("RU-TH-BANK")
	if !ok {
		t.Fatal("Tariff RU-TH-BANK: want ok")
	}
	src := mustCur(t, cat, "RUB")
	tgt := mustCur(t, cat, "THB")
	target := decimal.RequireFromString("1")

	for _, rateStr := range []string{"0", "-1"} {
		rate := decimal.RequireFromString(rateStr)
		got, err := Inverse(target, rate, tf.Fee, src, tgt)
		if err == nil {
			t.Fatalf("rate %s: want error", rateStr)
		}
		if !strings.Contains(err.Error(), "rate") {
			t.Fatalf("rate %s: error %q must contain rate", rateStr, err.Error())
		}
		assertZeroForwardQuote(t, got)
	}
}

func TestInverse_err_unreachable(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(100),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "SOURCE",
	}
	rate := decimal.RequireFromString("0.42")
	target := decimal.RequireFromString("1")

	got, err := Inverse(target, rate, rule, rub, thb)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("error %q must contain unreachable", err.Error())
	}
	assertZeroForwardQuote(t, got)
}

func TestInverse_err_charged_in(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(1),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "BOTH",
	}
	got, err := Inverse(decimal.RequireFromString("10"), decimal.RequireFromString("1"), rule, rub, thb)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "charged_in") {
		t.Fatalf("error %q must contain charged_in", err.Error())
	}
	assertZeroForwardQuote(t, got)
}

func TestInverse_err_empty_currency(t *testing.T) {
	cat := loadFeeCatalog(t)
	thb := mustCur(t, cat, "THB")
	rub := refdata.Currency{Code: "", MinorUnits: 2}
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(1),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "SOURCE",
	}
	got, err := Inverse(decimal.RequireFromString("10"), decimal.RequireFromString("1"), rule, rub, thb)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "currency") {
		t.Fatalf("error %q must contain currency", err.Error())
	}
	assertZeroForwardQuote(t, got)
}
