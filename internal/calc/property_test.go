package calc

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestInverse_roundTrip_withinOneSourceStep(t *testing.T) {
	cat := loadFeeCatalog(t)
	cases := []struct {
		name, direction        string
		source, rate           string
		srcCur, feeCur, tgtCur string
	}{
		{name: "ex01_RU_TH_BANK", direction: "RU-TH-BANK", source: "5000", rate: "0.42", srcCur: "RUB", feeCur: "RUB", tgtCur: "THB"},
		{name: "ex02_RU_TH_BANK", direction: "RU-TH-BANK", source: "1100", rate: "0.42", srcCur: "RUB", feeCur: "RUB", tgtCur: "THB"},
		{name: "ex03_RU_TH_BANK", direction: "RU-TH-BANK", source: "299900", rate: "0.42", srcCur: "RUB", feeCur: "RUB", tgtCur: "THB"},
		{name: "ex07_RU_TH_CARD", direction: "RU-TH-CARD", source: "2500", rate: "0.42", srcCur: "RUB", feeCur: "RUB", tgtCur: "THB"},
		{name: "ex08_RU_JP_BANK", direction: "RU-JP-BANK", source: "10000", rate: "1.9", srcCur: "RUB", feeCur: "RUB", tgtCur: "JPY"},
		{name: "ex09_KW_TH_BANK", direction: "KW-TH-BANK", source: "50", rate: "113.75", srcCur: "KWD", feeCur: "KWD", tgtCur: "THB"},
		{name: "ex10_RU_VN_BANK", direction: "RU-VN-BANK", source: "7000", rate: "300", srcCur: "RUB", feeCur: "VND", tgtCur: "VND"},
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

			f, err := Forward(decimal.RequireFromString(tt.source), rate, tf.Fee, src, tgt)
			if err != nil {
				t.Fatalf("Forward: %v", err)
			}
			q, err := Inverse(f.TargetAmount.Amount, rate, tf.Fee, src, tgt)
			if err != nil {
				t.Fatalf("Inverse: %v", err)
			}
			if !q.TargetAmount.Amount.Equal(f.TargetAmount.Amount) || q.TargetAmount.Currency != f.TargetAmount.Currency {
				t.Errorf("TargetAmount: got %s %s want %s %s", q.TargetAmount.Amount, q.TargetAmount.Currency, f.TargetAmount.Amount, f.TargetAmount.Currency)
			}
			step := decimal.New(1, -int32(src.MinorUnits))
			srcDiff := q.SourceAmount.Amount.Sub(f.SourceAmount.Amount).Abs()
			if srcDiff.GreaterThan(step) {
				t.Errorf("|Inverse source - Forward source| = %s > step %s", srcDiff, step)
			}

			fq, err := Forward(q.SourceAmount.Amount, rate, tf.Fee, src, tgt)
			if err != nil {
				t.Fatalf("Forward(Inverse source): %v", err)
			}
			assertMoneyParts(t, q.SourceAmount, src, fq.SourceAmount.Amount.StringFixed(int32(src.MinorUnits)))
			assertMoneyParts(t, q.Fee, feeCur, fq.Fee.Amount.StringFixed(int32(feeCur.MinorUnits)))
			assertMoneyParts(t, q.AmountToConvert, src, fq.AmountToConvert.Amount.StringFixed(int32(src.MinorUnits)))
			assertMoneyParts(t, q.TargetAmount, tgt, fq.TargetAmount.Amount.StringFixed(int32(tgt.MinorUnits)))
		})
	}
}
