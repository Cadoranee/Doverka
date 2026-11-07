package rates

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/shopspring/decimal"
)

type memSource struct {
	rates []Rate
	err   error
	calls int
}

func (m *memSource) Fetch(ctx context.Context) ([]Rate, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	out := make([]Rate, len(m.rates))
	copy(out, m.rates)
	return out, nil
}

type countingSource struct {
	calls int
}

func (c *countingSource) Fetch(ctx context.Context) ([]Rate, error) {
	c.calls++
	return nil, errors.New("should not fetch")
}

func loadRatesJSON(t *testing.T) []Rate {
	t.Helper()
	path := filepath.Join("..", "..", "data", "rates.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rates, err := parseRatesJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	return rates
}

func mustRate(t *testing.T, base, quote string, want string, rates []Rate, now time.Time) {
	t.Helper()
	svc := New(&memSource{rates: rates}, clock.NewFake(now))
	got, err := svc.Rate(context.Background(), base, quote)
	if err != nil {
		t.Fatalf("%s→%s: %v", base, quote, err)
	}
	wantD := decimal.RequireFromString(want)
	if !got.Equal(wantD) {
		t.Fatalf("%s→%s: got %s want %s", base, quote, got, wantD)
	}
}

func mustUnavailable(t *testing.T, base, quote string, rates []Rate, now time.Time) {
	t.Helper()
	svc := New(&memSource{rates: rates}, clock.NewFake(now))
	_, err := svc.Rate(context.Background(), base, quote)
	if err == nil {
		t.Fatalf("%s→%s: expected error", base, quote)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%s→%s: %v", base, quote, err)
	}
}

func TestDirectFromRatesJSON(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rates := loadRatesJSON(t)
	mustRate(t, "RUB", "THB", "0.4200", rates, now)
	mustRate(t, "RUB", "JPY", "1.9000", rates, now)
	mustRate(t, "RUB", "VND", "300.0000", rates, now)
	mustRate(t, "KWD", "THB", "113.7500", rates, now)
}

func TestStalePairsUnavailable(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rates := loadRatesJSON(t)
	mustUnavailable(t, "RUB", "PHP", rates, now)
	mustUnavailable(t, "RUB", "AED", rates, now)
}

func TestFreshnessBoundary(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	freshAt := now.Add(-StaleAfter)
	staleAt := now.Add(-StaleAfter - time.Nanosecond)
	rates := []Rate{
		{Base: "AAA", Quote: "BBB", Value: decimal.RequireFromString("1.5"), UpdatedAt: freshAt},
	}
	mustRate(t, "AAA", "BBB", "1.5", rates, now)
	rates[0].UpdatedAt = staleAt
	mustUnavailable(t, "AAA", "BBB", rates, now)
}

func TestInversePreferredOverCross(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rates := []Rate{
		{Base: "KWD", Quote: "RUB", Value: decimal.RequireFromString("270.0000"), UpdatedAt: now},
		{Base: "RUB", Quote: "EUR", Value: decimal.RequireFromString("0.0110"), UpdatedAt: now},
		{Base: "EUR", Quote: "KWD", Value: decimal.RequireFromString("0.3300"), UpdatedAt: now},
	}
	mustRate(t, "RUB", "KWD", "0.0037037037037037037037037037", rates, now)
}

func TestCrossOneHop(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rates := []Rate{
		{Base: "EUR", Quote: "KWD", Value: decimal.RequireFromString("0.3300"), UpdatedAt: now},
		{Base: "KWD", Quote: "THB", Value: decimal.RequireFromString("113.7500"), UpdatedAt: now},
	}
	svc := New(&memSource{rates: rates}, clock.NewFake(now))
	got, err := svc.Rate(context.Background(), "EUR", "THB")
	if err != nil {
		t.Fatal(err)
	}
	want := decimal.RequireFromString("37.5375")
	if !got.Equal(want) {
		t.Fatalf("rate: got %s want %s", got, want)
	}
	conv, err := svc.Convert(context.Background(), decimal.RequireFromString("2"), "EUR", "THB")
	if err != nil {
		t.Fatal(err)
	}
	wantConv := decimal.RequireFromString("75.075")
	if !conv.Equal(wantConv) {
		t.Fatalf("convert: got %s want %s", conv, wantConv)
	}
}

func TestSliceOrderFirstHopWins(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rates := []Rate{
		{Base: "EUR", Quote: "USD", Value: decimal.RequireFromString("2"), UpdatedAt: now},
		{Base: "USD", Quote: "THB", Value: decimal.RequireFromString("10"), UpdatedAt: now},
		{Base: "EUR", Quote: "KWD", Value: decimal.RequireFromString("3"), UpdatedAt: now},
		{Base: "KWD", Quote: "THB", Value: decimal.RequireFromString("10"), UpdatedAt: now},
	}
	mustRate(t, "EUR", "THB", "20", rates, now)
}

func TestDirectBeatsInverse(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rates := []Rate{
		{Base: "RUB", Quote: "THB", Value: decimal.RequireFromString("0.42"), UpdatedAt: now},
		{Base: "THB", Quote: "RUB", Value: decimal.RequireFromString("10"), UpdatedAt: now},
	}
	mustRate(t, "RUB", "THB", "0.42", rates, now)
}

func TestUnusablePublishedPairBlocksFallback(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-StaleAfter - time.Hour)
	mustUnavailable(t, "RUB", "THB", []Rate{
		{Base: "RUB", Quote: "THB", Value: decimal.RequireFromString("5"), UpdatedAt: stale},
		{Base: "THB", Quote: "RUB", Value: decimal.RequireFromString("10"), UpdatedAt: now},
	}, now)
	mustUnavailable(t, "RUB", "PHP", []Rate{
		{Base: "RUB", Quote: "PHP", Value: decimal.RequireFromString("0.7000"), UpdatedAt: stale},
		{Base: "RUB", Quote: "USD", Value: decimal.RequireFromString("0.0120"), UpdatedAt: now},
		{Base: "USD", Quote: "PHP", Value: decimal.RequireFromString("58"), UpdatedAt: now},
	}, now)
	mustUnavailable(t, "EUR", "THB", []Rate{
		{Base: "THB", Quote: "EUR", Value: decimal.RequireFromString("0.5"), UpdatedAt: stale},
		{Base: "EUR", Quote: "KWD", Value: decimal.RequireFromString("0.3300"), UpdatedAt: now},
		{Base: "KWD", Quote: "THB", Value: decimal.RequireFromString("113.7500"), UpdatedAt: now},
	}, now)
}

func TestStaleHopSkippedUsesNext(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-StaleAfter - time.Hour)
	rates := []Rate{
		{Base: "EUR", Quote: "USD", Value: decimal.RequireFromString("2"), UpdatedAt: stale},
		{Base: "USD", Quote: "THB", Value: decimal.RequireFromString("10"), UpdatedAt: now},
		{Base: "EUR", Quote: "KWD", Value: decimal.RequireFromString("3"), UpdatedAt: now},
		{Base: "KWD", Quote: "THB", Value: decimal.RequireFromString("10"), UpdatedAt: now},
	}
	mustRate(t, "EUR", "THB", "30", rates, now)
}

func TestSameCurrencyNoFetch(t *testing.T) {
	src := &countingSource{}
	svc := New(src, clock.NewFake(time.Now().UTC()))
	got, err := svc.Rate(context.Background(), "RUB", "RUB")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("rate: %s", got)
	}
	amt := decimal.RequireFromString("123.45")
	conv, err := svc.Convert(context.Background(), amt, "RUB", "RUB")
	if err != nil {
		t.Fatal(err)
	}
	if !conv.Equal(amt) {
		t.Fatalf("convert: %s", conv)
	}
	if src.calls != 0 {
		t.Fatalf("fetch calls: %d", src.calls)
	}
}

func TestEmptyCurrencyNoFetch(t *testing.T) {
	src := &memSource{}
	svc := New(src, clock.NewFake(time.Now().UTC()))
	for _, pair := range [][2]string{{"", "THB"}, {"RUB", ""}, {" ", "THB"}, {"RUB", "  "}} {
		_, err := svc.Rate(context.Background(), pair[0], pair[1])
		if err == nil || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("pair %q,%q: %v", pair[0], pair[1], err)
		}
	}
	if src.calls != 0 {
		t.Fatalf("fetch calls: %d", src.calls)
	}
}

func TestConvertAmounts(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rates := loadRatesJSON(t)
	svc := New(&memSource{rates: rates}, clock.NewFake(now))
	conv, err := svc.Convert(context.Background(), decimal.RequireFromString("100"), "RUB", "THB")
	if err != nil {
		t.Fatal(err)
	}
	if !conv.Equal(decimal.RequireFromString("42")) {
		t.Fatalf("RUB→THB: %s", conv)
	}
	conv, err = svc.Convert(context.Background(), decimal.RequireFromString("50"), "KWD", "THB")
	if err != nil {
		t.Fatal(err)
	}
	if !conv.Equal(decimal.RequireFromString("5687.5")) {
		t.Fatalf("KWD→THB: %s", conv)
	}
}

func TestHTTPSourceSuccess(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	body := `[{"base":"RUB","quote":"THB","rate":"0.4200","updated_at":"2026-11-05T11:46:06Z"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rates" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	hsrc := NewHTTPSource(srv.URL, nil)
	svc := New(hsrc, clock.NewFake(now))
	got, err := svc.Rate(context.Background(), "RUB", "THB")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(decimal.RequireFromString("0.4200")) {
		t.Fatalf("got %s", got)
	}
}

func TestHTTPSource500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer srv.Close()
	svc := New(NewHTTPSource(srv.URL, nil), clock.NewFake(time.Now().UTC()))
	_, err := svc.Rate(context.Background(), "RUB", "THB")
	if err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err: %v", err)
	}
}

func TestHTTPSourceInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	svc := New(NewHTTPSource(srv.URL, nil), clock.NewFake(time.Now().UTC()))
	_, err := svc.Rate(context.Background(), "RUB", "THB")
	if err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err: %v", err)
	}
}

func TestNonPositiveRateUnavailable(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, val := range []string{"0", "-1"} {
		rates := []Rate{
			{Base: "RUB", Quote: "THB", Value: decimal.RequireFromString(val), UpdatedAt: now},
		}
		mustUnavailable(t, "RUB", "THB", rates, now)
	}
}

func TestFutureUpdatedAtFresh(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	rates := []Rate{
		{Base: "RUB", Quote: "THB", Value: decimal.RequireFromString("0.5"), UpdatedAt: future},
	}
	mustRate(t, "RUB", "THB", "0.5", rates, now)
}

func TestHTTPSourceTrimBaseURL(t *testing.T) {
	body := `[{"base":"X","quote":"Y","rate":"1.25","updated_at":"2026-11-05T12:00:00Z"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rates") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	hsrc := NewHTTPSource(srv.URL+"/", nil)
	rates, err := hsrc.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || !rates[0].Value.Equal(decimal.RequireFromString("1.25")) {
		t.Fatalf("%+v", rates)
	}
}

func TestFetchErrorWrapped(t *testing.T) {
	src := &memSource{err: errors.New("network down")}
	svc := New(src, clock.NewFake(time.Now().UTC()))
	_, err := svc.Rate(context.Background(), "RUB", "THB")
	if err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(err.Error(), "network down") {
		t.Fatalf("err: %v", err)
	}
}
