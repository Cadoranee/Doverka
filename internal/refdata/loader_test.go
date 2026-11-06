package refdata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func writeMinimal(t *testing.T, dir string) {
	t.Helper()
	cur := `[{"code":"RUB","minor_units":2}]`
	tar := `[{
  "direction_id": "RU-TH-BANK",
  "source": { "country": "RU", "currency": "RUB" },
  "target": { "country": "TH", "currency": "THB" },
  "payout_method": "BANK_ACCOUNT",
  "fee": {
    "percent": "1.50",
    "fixed": "0.00",
    "min": "50.00",
    "charged_in": "SOURCE"
  },
  "amount_limits": {
    "min": "1000.00",
    "max": "300000.00",
    "currency": "RUB"
  },
  "min_margin_percent": "0.50",
  "quote_ttl_sec": 900
}]`
	clients := `[{
  "client_id": "c_001",
  "balances": { "RUB": "500000.00" },
  "daily_limit": { "amount": "300000.00", "currency": "RUB" }
}]`
	if err := os.WriteFile(filepath.Join(dir, "currencies.json"), []byte(cur), 0o644); err != nil {
		t.Fatalf("write currencies.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tariffs.json"), []byte(tar), 0o644); err != nil {
		t.Fatalf("write tariffs.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clients.json"), []byte(clients), 0o644); err != nil {
		t.Fatalf("write clients.json: %v", err)
	}
}

func TestLoad_repoData(t *testing.T) {
	cat, err := Load("../../data")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ruThBank, ok := cat.Tariff("RU-TH-BANK")
	if !ok {
		t.Fatal("Tariff RU-TH-BANK: want ok true")
	}
	if ruThBank.Source.Country != "RU" || ruThBank.Source.Currency != "RUB" {
		t.Errorf("RU-TH-BANK source: got %+v", ruThBank.Source)
	}
	if ruThBank.Target.Country != "TH" || ruThBank.Target.Currency != "THB" {
		t.Errorf("RU-TH-BANK target: got %+v", ruThBank.Target)
	}
	if ruThBank.PayoutMethod != "BANK_ACCOUNT" {
		t.Errorf("PayoutMethod: got %q", ruThBank.PayoutMethod)
	}
	if ruThBank.Fee.ChargedIn != "SOURCE" {
		t.Errorf("Fee.ChargedIn: got %q", ruThBank.Fee.ChargedIn)
	}
	if !ruThBank.Fee.Percent.Equal(decimal.RequireFromString("1.50")) {
		t.Errorf("Fee.Percent: got %v", ruThBank.Fee.Percent)
	}
	if !ruThBank.Fee.Fixed.Equal(decimal.RequireFromString("0.00")) {
		t.Errorf("Fee.Fixed: got %v", ruThBank.Fee.Fixed)
	}
	if !ruThBank.Fee.Min.Equal(decimal.RequireFromString("50.00")) {
		t.Errorf("Fee.Min: got %v", ruThBank.Fee.Min)
	}
	if !ruThBank.AmountLimits.Min.Equal(decimal.RequireFromString("1000.00")) {
		t.Errorf("AmountLimits.Min: got %v", ruThBank.AmountLimits.Min)
	}
	if !ruThBank.AmountLimits.Max.Equal(decimal.RequireFromString("300000.00")) {
		t.Errorf("AmountLimits.Max: got %v", ruThBank.AmountLimits.Max)
	}
	if ruThBank.AmountLimits.Currency != "RUB" {
		t.Errorf("AmountLimits.Currency: got %q", ruThBank.AmountLimits.Currency)
	}
	if !ruThBank.MinMarginPercent.Equal(decimal.RequireFromString("0.50")) {
		t.Errorf("MinMarginPercent: got %v", ruThBank.MinMarginPercent)
	}
	if ruThBank.QuoteTTLSec != 900 {
		t.Errorf("QuoteTTLSec: got %d", ruThBank.QuoteTTLSec)
	}

	ruVn, ok := cat.Tariff("RU-VN-BANK")
	if !ok {
		t.Fatal("Tariff RU-VN-BANK: want ok true")
	}
	if ruVn.Fee.ChargedIn != "TARGET" {
		t.Errorf("RU-VN-BANK ChargedIn: got %q", ruVn.Fee.ChargedIn)
	}
	if !ruVn.Fee.Fixed.Equal(decimal.RequireFromString("5000")) {
		t.Errorf("RU-VN-BANK Fee.Fixed: got %v", ruVn.Fee.Fixed)
	}
	if !ruVn.Fee.Min.Equal(decimal.RequireFromString("30000")) {
		t.Errorf("RU-VN-BANK Fee.Min: got %v", ruVn.Fee.Min)
	}

	rub, ok := cat.Currency("RUB")
	if !ok || rub.MinorUnits != 2 || rub.Code != "RUB" {
		t.Errorf("Currency RUB: got %+v ok=%v", rub, ok)
	}
	jpy, ok := cat.Currency("JPY")
	if !ok || jpy.MinorUnits != 0 || jpy.Code != "JPY" {
		t.Errorf("Currency JPY: got %+v ok=%v", jpy, ok)
	}
	kwd, ok := cat.Currency("KWD")
	if !ok || kwd.MinorUnits != 3 || kwd.Code != "KWD" {
		t.Errorf("Currency KWD: got %+v ok=%v", kwd, ok)
	}
	if _, ok := cat.Currency("___"); ok {
		t.Error("Currency missing code: want false")
	}

	c1, ok := cat.Client("c_001")
	if !ok {
		t.Fatal("Client c_001: want ok true")
	}
	if !c1.Balances["RUB"].Equal(decimal.RequireFromString("500000.00")) {
		t.Errorf("c_001 RUB balance: got %v", c1.Balances["RUB"])
	}
	if !c1.DailyLimit.Amount.Equal(decimal.RequireFromString("300000.00")) {
		t.Errorf("c_001 daily limit amount: got %v", c1.DailyLimit.Amount)
	}
	if c1.DailyLimit.Currency != "RUB" {
		t.Errorf("c_001 daily limit currency: got %q", c1.DailyLimit.Currency)
	}

	c6, ok := cat.Client("c_006")
	if !ok {
		t.Fatal("Client c_006: want ok true")
	}
	if !c6.Balances["RUB"].Equal(decimal.RequireFromString("750000.00")) {
		t.Errorf("c_006 RUB: got %v", c6.Balances["RUB"])
	}
	if !c6.Balances["KWD"].Equal(decimal.RequireFromString("2500.000")) {
		t.Errorf("c_006 KWD: got %v", c6.Balances["KWD"])
	}

	c3, ok := cat.Client("c_003")
	if !ok {
		t.Fatal("Client c_003: want ok true")
	}
	if c3.DailyLimit.Currency != "USD" {
		t.Errorf("c_003 daily limit currency: got %q", c3.DailyLimit.Currency)
	}
	if !c3.DailyLimit.Amount.Equal(decimal.RequireFromString("5000.00")) {
		t.Errorf("c_003 daily limit amount: got %v", c3.DailyLimit.Amount)
	}

	if _, ok := cat.Client("unknown"); ok {
		t.Error("unknown client: want false")
	}

	ft1, ok := cat.FindTariff("RU", "RUB", "TH", "THB", "BANK_ACCOUNT")
	if !ok || ft1.DirectionID != "RU-TH-BANK" {
		t.Errorf("FindTariff BANK_ACCOUNT: got %+v ok=%v", ft1, ok)
	}
	ft2, ok := cat.FindTariff("RU", "RUB", "TH", "THB", "CARD")
	if !ok || ft2.DirectionID != "RU-TH-CARD" {
		t.Errorf("FindTariff CARD: got %+v ok=%v", ft2, ok)
	}
	if _, ok := cat.FindTariff("RU", "RUB", "TH", "THB", "CASH_PICKUP"); ok {
		t.Error("FindTariff CASH_PICKUP RU-TH: want false")
	}

	first, ok := cat.Client("c_001")
	if !ok {
		t.Fatal("Client c_001 second fetch: want ok")
	}
	first.Balances["RUB"] = decimal.RequireFromString("0")
	second, ok := cat.Client("c_001")
	if !ok {
		t.Fatal("Client c_001 after mutate: want ok")
	}
	if !second.Balances["RUB"].Equal(decimal.RequireFromString("500000.00")) {
		t.Errorf("after mutating copy, Client balance: got %v", second.Balances["RUB"])
	}
	if first.Balances["RUB"].Equal(second.Balances["RUB"]) {
		t.Error("mutated copy should differ from catalog value")
	}
}

func TestLoad_emptyDir(t *testing.T) {
	_, err := Load("  ")
	if err == nil {
		t.Fatal("want error")
	}
}

func TestLoad_duplicateRoute(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	dup := `[{"direction_id":"A","source":{"country":"RU","currency":"RUB"},"target":{"country":"TH","currency":"THB"},"payout_method":"BANK_ACCOUNT","fee":{"percent":"1","fixed":"0","min":"1","charged_in":"SOURCE"},"amount_limits":{"min":"1","max":"2","currency":"RUB"},"min_margin_percent":"0","quote_ttl_sec":1},{"direction_id":"B","source":{"country":"RU","currency":"RUB"},"target":{"country":"TH","currency":"THB"},"payout_method":"BANK_ACCOUNT","fee":{"percent":"1","fixed":"0","min":"1","charged_in":"SOURCE"},"amount_limits":{"min":"1","max":"2","currency":"RUB"},"min_margin_percent":"0","quote_ttl_sec":1}]`
	if err := os.WriteFile(filepath.Join(dir, "tariffs.json"), []byte(dup), 0o644); err != nil {
		t.Fatalf("write tariffs: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "duplicate tariff route") {
		t.Errorf("error %q must mention duplicate tariff route", err.Error())
	}
}

func TestLoad_missingTariffsFile(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	if err := os.Remove(filepath.Join(dir, "tariffs.json")); err != nil {
		t.Fatalf("remove tariffs: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "tariffs.json") {
		t.Errorf("error %q must mention tariffs.json", err.Error())
	}
}

func TestLoad_invalidJSON(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "tariffs.json"), []byte(`{`), 0o644); err != nil {
		t.Fatalf("write bad json: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestLoad_duplicateDirectionID(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	dup := `[{"direction_id":"RU-TH-BANK","source":{"country":"RU","currency":"RUB"},"target":{"country":"X","currency":"Y"},"payout_method":"BANK_ACCOUNT","fee":{"percent":"1","fixed":"0","min":"1","charged_in":"SOURCE"},"amount_limits":{"min":"1","max":"2","currency":"RUB"},"min_margin_percent":"0","quote_ttl_sec":1},{"direction_id":"RU-TH-BANK","source":{"country":"RU","currency":"RUB"},"target":{"country":"X","currency":"Y"},"payout_method":"BANK_ACCOUNT","fee":{"percent":"1","fixed":"0","min":"1","charged_in":"SOURCE"},"amount_limits":{"min":"1","max":"2","currency":"RUB"},"min_margin_percent":"0","quote_ttl_sec":1}]`
	if err := os.WriteFile(filepath.Join(dir, "tariffs.json"), []byte(dup), 0o644); err != nil {
		t.Fatalf("write tariffs: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "direction_id") {
		t.Errorf("error %q must mention direction_id", err.Error())
	}
}

func TestLoad_duplicateCurrencyCode(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "currencies.json"), []byte(`[{"code":"RUB","minor_units":2},{"code":"RUB","minor_units":2}]`), 0o644); err != nil {
		t.Fatalf("write currencies: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestLoad_duplicateClientID(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	dup := `[{"client_id":"c_001","balances":{"RUB":"1"},"daily_limit":{"amount":"1","currency":"RUB"}},{"client_id":"c_001","balances":{"RUB":"1"},"daily_limit":{"amount":"1","currency":"RUB"}}]`
	if err := os.WriteFile(filepath.Join(dir, "clients.json"), []byte(dup), 0o644); err != nil {
		t.Fatalf("write clients: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestLoad_badDecimalFeePercent(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	bad := `[{
  "direction_id": "RU-TH-BANK",
  "source": { "country": "RU", "currency": "RUB" },
  "target": { "country": "TH", "currency": "THB" },
  "payout_method": "BANK_ACCOUNT",
  "fee": {
    "percent": "not-a-decimal",
    "fixed": "0.00",
    "min": "50.00",
    "charged_in": "SOURCE"
  },
  "amount_limits": { "min": "1000.00", "max": "300000.00", "currency": "RUB" },
  "min_margin_percent": "0.50",
  "quote_ttl_sec": 900
}]`
	if err := os.WriteFile(filepath.Join(dir, "tariffs.json"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write tariffs: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestLoad_chargedInBothRejected(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	bad := `[{
  "direction_id": "RU-TH-BANK",
  "source": { "country": "RU", "currency": "RUB" },
  "target": { "country": "TH", "currency": "THB" },
  "payout_method": "BANK_ACCOUNT",
  "fee": {
    "percent": "1.50",
    "fixed": "0.00",
    "min": "50.00",
    "charged_in": "BOTH"
  },
  "amount_limits": { "min": "1000.00", "max": "300000.00", "currency": "RUB" },
  "min_margin_percent": "0.50",
  "quote_ttl_sec": 900
}]`
	if err := os.WriteFile(filepath.Join(dir, "tariffs.json"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write tariffs: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "charged_in") {
		t.Errorf("error %q must mention charged_in", err.Error())
	}
}

func TestLoad_minorUnitsNegative(t *testing.T) {
	dir := t.TempDir()
	writeMinimal(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "currencies.json"), []byte(`[{"code":"RUB","minor_units":-1}]`), 0o644); err != nil {
		t.Fatalf("write currencies: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "minor_units") {
		t.Errorf("error %q must mention minor_units", err.Error())
	}
}
