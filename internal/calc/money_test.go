package calc

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

func TestRound(t *testing.T) {
	cat, err := refdata.Load(filepath.Join("..", "..", "data"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rub, ok := cat.Currency("RUB")
	if !ok || rub.Code != "RUB" || rub.MinorUnits != 2 {
		t.Fatalf("catalog RUB: got %+v ok=%v", rub, ok)
	}
	thb, ok := cat.Currency("THB")
	if !ok || thb.Code != "THB" || thb.MinorUnits != 2 {
		t.Fatalf("catalog THB: got %+v ok=%v", thb, ok)
	}
	jpy, ok := cat.Currency("JPY")
	if !ok || jpy.Code != "JPY" || jpy.MinorUnits != 0 {
		t.Fatalf("catalog JPY: got %+v ok=%v", jpy, ok)
	}
	kwd, ok := cat.Currency("KWD")
	if !ok || kwd.Code != "KWD" || kwd.MinorUnits != 3 {
		t.Fatalf("catalog KWD: got %+v ok=%v", kwd, ok)
	}
	vnd, ok := cat.Currency("VND")
	if !ok || vnd.Code != "VND" || vnd.MinorUnits != 0 {
		t.Fatalf("catalog VND: got %+v ok=%v", vnd, ok)
	}

	halfUp := []struct {
		name string
		code string
		in   string
		mul  string
		want string
	}{
		{name: "rub_10.005", code: "RUB", in: "10.005", want: "10.01"},
		{name: "rub_10.004", code: "RUB", in: "10.004", want: "10.00"},
		{name: "rub_10.015", code: "RUB", in: "10.015", want: "10.02"},
		{name: "rub_1.225", code: "RUB", in: "1.225", want: "1.23"},
		{name: "rub_1.224", code: "RUB", in: "1.224", want: "1.22"},
		{name: "rub_1.2251", code: "RUB", in: "1.2251", want: "1.23"},
		{name: "rub_0.005", code: "RUB", in: "0.005", want: "0.01"},
		{name: "rub_-1.225", code: "RUB", in: "-1.225", want: "-1.23"},
		{name: "rub_75", code: "RUB", in: "75", want: "75.00"},
		{name: "jpy_18658.5", code: "JPY", in: "18658.5", want: "18659"},
		{name: "jpy_18658.4", code: "JPY", in: "18658.4", want: "18658"},
		{name: "jpy_1.5", code: "JPY", in: "1.5", want: "2"},
		{name: "jpy_-1.5", code: "JPY", in: "-1.5", want: "-2"},
		{name: "jpy_-0.5", code: "JPY", in: "-0.5", want: "-1"},
		{name: "kwd_1.2345", code: "KWD", in: "1.2345", want: "1.235"},
		{name: "kwd_1.2344", code: "KWD", in: "1.2344", want: "1.234"},
		{name: "kwd_1.23451", code: "KWD", in: "1.23451", want: "1.235"},
		{name: "kwd_-1.2345", code: "KWD", in: "-1.2345", want: "-1.235"},
		{name: "vnd_30200.5", code: "VND", in: "30200.5", want: "30201"},
		{name: "vnd_30200.4", code: "VND", in: "30200.4", want: "30200"},
		{name: "vnd_-0.5", code: "VND", in: "-0.5", want: "-1"},
		{name: "ex01_thb", code: "THB", in: "4925", mul: "0.42", want: "2068.50"},
		{name: "ex08_jpy", code: "JPY", in: "9820", mul: "1.9", want: "18658"},
		{name: "ex09_thb", code: "THB", in: "49", mul: "113.75", want: "5573.75"},
	}
	for _, tt := range halfUp {
		t.Run(tt.name, func(t *testing.T) {
			cur, ok := cat.Currency(tt.code)
			if !ok {
				t.Fatalf("Currency(%q): want ok", tt.code)
			}
			in := decimal.RequireFromString(tt.in)
			if tt.mul != "" {
				in = in.Mul(decimal.RequireFromString(tt.mul))
			}
			got, err := Round(in, cur)
			if err != nil {
				t.Fatalf("Round: %v", err)
			}
			assertRounded(t, got, cur, tt.want)
		})
	}

	t.Run("err_empty_code", func(t *testing.T) {
		got, err := Round(decimal.RequireFromString("1"), refdata.Currency{Code: "", MinorUnits: 2})
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "currency") {
			t.Errorf("error %q must contain currency", err.Error())
		}
		if got.Currency != "" || !got.Amount.IsZero() {
			t.Errorf("got %+v, want zero Money", got)
		}
	})

	t.Run("err_negative_minor", func(t *testing.T) {
		got, err := Round(decimal.RequireFromString("1"), refdata.Currency{Code: "RUB", MinorUnits: -1})
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "minor_units") {
			t.Errorf("error %q must contain minor_units", err.Error())
		}
		if got.Currency != "" || !got.Amount.IsZero() {
			t.Errorf("got %+v, want zero Money", got)
		}
	})
}

func assertRounded(t *testing.T, got Money, cur refdata.Currency, want string) {
	t.Helper()
	expected := decimal.RequireFromString(want)
	if got.Currency != cur.Code {
		t.Errorf("Currency: got %q want %q", got.Currency, cur.Code)
	}
	if !got.Amount.Equal(expected) {
		t.Errorf("Amount: got %s want %s", got.Amount, expected)
	}
	wantExp := -int32(cur.MinorUnits)
	if got.Amount.Exponent() != wantExp {
		t.Errorf("Exponent: got %d want %d", got.Amount.Exponent(), wantExp)
	}
}
