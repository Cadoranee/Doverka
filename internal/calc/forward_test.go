package calc

import (
	"strings"
	"testing"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

func TestForward_examples(t *testing.T) {
	cat := loadFeeCatalog(t)
	cases := []struct {
		name, direction            string
		source, rate               string
		srcCur, feeCur, tgtCur     string
		wantSrc, wantFee, wantConv string
		wantTgt                    string
	}{
		{
			name: "ex01_RU_TH_BANK", direction: "RU-TH-BANK",
			source: "5000", rate: "0.42",
			srcCur: "RUB", feeCur: "RUB", tgtCur: "THB",
			wantSrc: "5000.00", wantFee: "75.00", wantConv: "4925.00", wantTgt: "2068.50",
		},
		{
			name: "ex02_RU_TH_BANK", direction: "RU-TH-BANK",
			source: "1100", rate: "0.42",
			srcCur: "RUB", feeCur: "RUB", tgtCur: "THB",
			wantSrc: "1100.00", wantFee: "50.00", wantConv: "1050.00", wantTgt: "441.00",
		},
		{
			name: "ex03_RU_TH_BANK", direction: "RU-TH-BANK",
			source: "299900", rate: "0.42",
			srcCur: "RUB", feeCur: "RUB", tgtCur: "THB",
			wantSrc: "299900.00", wantFee: "4498.50", wantConv: "295401.50", wantTgt: "124068.63",
		},
		{
			name: "ex07_RU_TH_CARD", direction: "RU-TH-CARD",
			source: "2500", rate: "0.42",
			srcCur: "RUB", feeCur: "RUB", tgtCur: "THB",
			wantSrc: "2500.00", wantFee: "80.00", wantConv: "2420.00", wantTgt: "1016.40",
		},
		{
			name: "ex08_RU_JP_BANK", direction: "RU-JP-BANK",
			source: "10000", rate: "1.9",
			srcCur: "RUB", feeCur: "RUB", tgtCur: "JPY",
			wantSrc: "10000.00", wantFee: "180.00", wantConv: "9820.00", wantTgt: "18658",
		},
		{
			name: "ex09_KW_TH_BANK", direction: "KW-TH-BANK",
			source: "50", rate: "113.75",
			srcCur: "KWD", feeCur: "KWD", tgtCur: "THB",
			wantSrc: "50.000", wantFee: "1.000", wantConv: "49.000", wantTgt: "5573.75",
		},
		{
			name: "ex10_RU_VN_BANK", direction: "RU-VN-BANK",
			source: "7000", rate: "300",
			srcCur: "RUB", feeCur: "VND", tgtCur: "VND",
			wantSrc: "7000.00", wantFee: "30200", wantConv: "7000.00", wantTgt: "2069800",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tf, ok := cat.Tariff(tt.direction)
			if !ok {
				t.Fatalf("Tariff %s: want ok", tt.direction)
			}
			src := mustCur(t, cat, tt.srcCur)
			feeCur := mustCur(t, cat, tt.feeCur)
			tgt := mustCur(t, cat, tt.tgtCur)
			rate := decimal.RequireFromString(tt.rate)
			got, err := Forward(decimal.RequireFromString(tt.source), rate, tf.Fee, src, tgt)
			if err != nil {
				t.Fatalf("Forward: %v", err)
			}
			assertForwardOK(t, got, rate, src, feeCur, src, tgt, tt.wantSrc, tt.wantFee, tt.wantConv, tt.wantTgt)
		})
	}
}

func TestForward_synthetic_half_up_target(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.Zero,
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "SOURCE",
	}
	rate := decimal.RequireFromString("0.33335")
	got, err := Forward(decimal.RequireFromString("100"), rate, rule, rub, thb)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	assertForwardOK(t, got, rate, rub, rub, rub, thb, "100.00", "0.00", "100.00", "33.34")
}

func TestForward_synthetic_target_fee_then_round(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(1),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "TARGET",
	}
	rate := decimal.RequireFromString("0.33335")
	got, err := Forward(decimal.RequireFromString("100"), rate, rule, rub, thb)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	assertForwardOK(t, got, rate, rub, thb, rub, thb, "100.00", "0.33", "100.00", "33.01")
}

func TestForward_err_charged_in(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	thb := mustCur(t, cat, "THB")
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(1),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "BOTH",
	}
	got, err := Forward(decimal.RequireFromString("100"), decimal.RequireFromString("1"), rule, rub, thb)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "charged_in") {
		t.Fatalf("error %q must contain charged_in", err.Error())
	}
	assertZeroForwardQuote(t, got)
}

func TestForward_err_empty_source_currency(t *testing.T) {
	cat := loadFeeCatalog(t)
	thb := mustCur(t, cat, "THB")
	rub := refdata.Currency{Code: "", MinorUnits: 2}
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(1),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "SOURCE",
	}
	got, err := Forward(decimal.RequireFromString("100"), decimal.RequireFromString("1"), rule, rub, thb)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "currency") {
		t.Fatalf("error %q must contain currency", err.Error())
	}
	assertZeroForwardQuote(t, got)
}

func TestForward_err_empty_target_currency(t *testing.T) {
	cat := loadFeeCatalog(t)
	rub := mustCur(t, cat, "RUB")
	target := refdata.Currency{Code: "", MinorUnits: 2}
	rule := refdata.Fee{
		Percent:   decimal.NewFromInt(1),
		Fixed:     decimal.Zero,
		Min:       decimal.Zero,
		ChargedIn: "SOURCE",
	}
	got, err := Forward(decimal.RequireFromString("100"), decimal.RequireFromString("1"), rule, rub, target)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "currency") {
		t.Fatalf("error %q must contain currency", err.Error())
	}
	assertZeroForwardQuote(t, got)
}

func assertForwardOK(t *testing.T, got ForwardQuote, rate decimal.Decimal, src, feeCur, convCur, tgt refdata.Currency, wantSrc, wantFee, wantConv, wantTgt string) {
	t.Helper()
	assertMoneyParts(t, got.SourceAmount, src, wantSrc)
	assertMoneyParts(t, got.Fee, feeCur, wantFee)
	assertMoneyParts(t, got.AmountToConvert, convCur, wantConv)
	assertMoneyParts(t, got.TargetAmount, tgt, wantTgt)
	if !got.Rate.Equal(rate) {
		t.Errorf("Rate: got %s want %s", got.Rate, rate)
	}
	if len(got.Breakdown) == 0 {
		t.Error("Breakdown: want non-empty")
	}
}

func assertZeroForwardQuote(t *testing.T, q ForwardQuote) {
	t.Helper()
	if q.SourceAmount.Currency != "" || !q.SourceAmount.Amount.IsZero() {
		t.Errorf("SourceAmount: got %+v want zero", q.SourceAmount)
	}
	if q.Fee.Currency != "" || !q.Fee.Amount.IsZero() {
		t.Errorf("Fee: got %+v want zero", q.Fee)
	}
	if q.AmountToConvert.Currency != "" || !q.AmountToConvert.Amount.IsZero() {
		t.Errorf("AmountToConvert: got %+v want zero", q.AmountToConvert)
	}
	if q.TargetAmount.Currency != "" || !q.TargetAmount.Amount.IsZero() {
		t.Errorf("TargetAmount: got %+v want zero", q.TargetAmount)
	}
	if !q.Rate.IsZero() {
		t.Errorf("Rate: got %s want zero", q.Rate)
	}
	if q.Breakdown != nil {
		t.Errorf("Breakdown: got %v want nil", q.Breakdown)
	}
}
