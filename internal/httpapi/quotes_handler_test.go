package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

var fixedNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type memStore struct {
	mu   sync.Mutex
	seq  int
	now  time.Time
	byID map[string]quote.Quote
}

func (s *memStore) Save(_ context.Context, snap quote.Snapshot, ttl time.Duration) (quote.Quote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := fmt.Sprintf("q-%d", s.seq)
	copySnap := quote.Snapshot{
		DirectionID:          snap.DirectionID,
		SourceCountry:        snap.SourceCountry,
		SourceCurrency:       snap.SourceCurrency,
		TargetCountry:        snap.TargetCountry,
		TargetCurrency:       snap.TargetCurrency,
		PayoutMethod:         snap.PayoutMethod,
		AmountType:           snap.AmountType,
		SourceAmountMinor:    snap.SourceAmountMinor,
		FeeAmountMinor:       snap.FeeAmountMinor,
		FeeCurrency:          snap.FeeCurrency,
		AmountToConvertMinor: snap.AmountToConvertMinor,
		Rate:                 snap.Rate,
		TargetAmountMinor:    snap.TargetAmountMinor,
		Breakdown:            append(json.RawMessage(nil), snap.Breakdown...),
	}
	if s.byID == nil {
		s.byID = make(map[string]quote.Quote)
	}
	q := quote.Quote{
		ID:        id,
		Snapshot:  copySnap,
		ExpiresAt: s.now.Add(ttl),
		CreatedAt: s.now,
	}
	s.byID[id] = q
	return q, nil
}

func (s *memStore) Get(_ context.Context, quoteID string) (quote.Quote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.byID[quoteID]
	if !ok {
		return quote.Quote{}, quote.ErrNotFound
	}
	return q, nil
}

func (s *memStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

type fakeRates struct {
	value decimal.Decimal
	err   error
}

func (f *fakeRates) Rate(_ context.Context, base, quoteCur string) (decimal.Decimal, error) {
	if f.err != nil {
		return decimal.Decimal{}, f.err
	}
	_ = base
	_ = quoteCur
	return f.value, nil
}

func loadCatalog(t *testing.T) *refdata.Catalog {
	t.Helper()
	cat, err := refdata.Load(filepath.Join("..", "..", "data"))
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	return cat
}

func exampleRequestBody(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "examples", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var wrapper struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	return wrapper.Request
}

func testHandler(t *testing.T, store *memStore, rateErr error) http.Handler {
	t.Helper()
	if store == nil {
		store = &memStore{now: fixedNow}
	}
	return (&Handler{
		Catalog: loadCatalog(t),
		Rates: &fakeRates{
			value: decimal.RequireFromString("0.4200"),
			err:   rateErr,
		},
		Store: store,
	}).Routes()
}

type apiErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type postQuoteBody struct {
	QuoteID         string          `json:"quote_id"`
	DirectionID     string          `json:"direction_id"`
	SourceAmount    moneyResponse   `json:"source_amount"`
	Fee             moneyResponse   `json:"fee"`
	AmountToConvert moneyResponse   `json:"amount_to_convert"`
	Rate            string          `json:"rate"`
	TargetAmount    moneyResponse   `json:"target_amount"`
	Breakdown       json.RawMessage `json:"breakdown"`
	ExpiresAt       string          `json:"expires_at"`
}

func postJSON(t *testing.T, h http.Handler, body []byte) (*httptest.ResponseRecorder, postQuoteBody, apiErrorBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/quotes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var ok postQuoteBody
		if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
			t.Fatalf("decode 201 body: %v raw=%s", err, rec.Body.String())
		}
		return rec, ok, apiErrorBody{}
	}
	var errBody apiErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body status=%d: %v raw=%s", rec.Code, err, rec.Body.String())
	}
	return rec, postQuoteBody{}, errBody
}

func getQuote(t *testing.T, h http.Handler, id string) (*httptest.ResponseRecorder, postQuoteBody, apiErrorBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/quotes/"+id, nil)
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var ok postQuoteBody
		if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
			t.Fatalf("decode 200 body: %v raw=%s", err, rec.Body.String())
		}
		return rec, ok, apiErrorBody{}
	}
	var errBody apiErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body status=%d: %v raw=%s", rec.Code, err, rec.Body.String())
	}
	return rec, postQuoteBody{}, errBody
}

func breakdownEqual(t *testing.T, a, b json.RawMessage) {
	t.Helper()
	var left, right []map[string]string
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("breakdown a: %v", err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("breakdown b: %v", err)
	}
	if len(left) != len(right) {
		t.Fatalf("breakdown len %d != %d", len(left), len(right))
	}
	for i := range left {
		if left[i]["step"] != right[i]["step"] {
			t.Fatalf("breakdown[%d] step %q != %q", i, left[i]["step"], right[i]["step"])
		}
	}
}

func TestPostQuote_sourceWithinLimits(t *testing.T) {
	h := testHandler(t, nil, nil)
	body := exampleRequestBody(t, "01_source_percent_fee.json")
	rec, resp, _ := postJSON(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp.DirectionID != "RU-TH-BANK" {
		t.Fatalf("direction_id=%q", resp.DirectionID)
	}
	if resp.SourceAmount.Amount != "5000.00" || resp.SourceAmount.Currency != "RUB" {
		t.Fatalf("source_amount=%+v", resp.SourceAmount)
	}
	if resp.Fee.Amount != "75.00" || resp.Fee.Currency != "RUB" {
		t.Fatalf("fee=%+v", resp.Fee)
	}
	if resp.AmountToConvert.Amount != "4925.00" || resp.AmountToConvert.Currency != "RUB" {
		t.Fatalf("amount_to_convert=%+v", resp.AmountToConvert)
	}
	if resp.Rate != "0.4200" {
		t.Fatalf("rate=%q", resp.Rate)
	}
	if resp.TargetAmount.Amount != "2068.50" || resp.TargetAmount.Currency != "THB" {
		t.Fatalf("target_amount=%+v", resp.TargetAmount)
	}
	if resp.QuoteID == "" {
		t.Fatal("quote_id empty")
	}
	wantExpires := fixedNow.Add(900 * time.Second).UTC().Format(time.RFC3339)
	if resp.ExpiresAt != wantExpires {
		t.Fatalf("expires_at=%q want %q", resp.ExpiresAt, wantExpires)
	}
	var steps []map[string]string
	if err := json.Unmarshal(resp.Breakdown, &steps); err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("breakdown empty")
	}
}

func TestPostQuote_targetAmountMatchesRequest(t *testing.T) {
	h := testHandler(t, nil, nil)
	body := exampleRequestBody(t, "03_target_mode.json")
	rec, resp, _ := postJSON(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if resp.TargetAmount.Amount != "124068.63" || resp.TargetAmount.Currency != "THB" {
		t.Fatalf("target_amount=%+v", resp.TargetAmount)
	}
	if resp.SourceAmount.Amount != "299900.00" || resp.SourceAmount.Currency != "RUB" {
		t.Fatalf("source_amount=%+v", resp.SourceAmount)
	}
	if resp.Fee.Amount != "4498.50" || resp.Fee.Currency != "RUB" {
		t.Fatalf("fee=%+v", resp.Fee)
	}
	if resp.AmountToConvert.Amount != "295401.50" || resp.AmountToConvert.Currency != "RUB" {
		t.Fatalf("amount_to_convert=%+v", resp.AmountToConvert)
	}
	if resp.Rate != "0.4200" {
		t.Fatalf("rate=%q", resp.Rate)
	}
	if resp.DirectionID != "RU-TH-BANK" {
		t.Fatalf("direction_id=%q", resp.DirectionID)
	}
}

func TestPostQuote_sameBodyNewQuoteID(t *testing.T) {
	h := testHandler(t, nil, nil)
	body := exampleRequestBody(t, "01_source_percent_fee.json")
	rec1, resp1, _ := postJSON(t, h, body)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	rec2, resp2, _ := postJSON(t, h, body)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if resp1.QuoteID == "" || resp2.QuoteID == "" {
		t.Fatal("quote_id empty")
	}
	if resp1.QuoteID == resp2.QuoteID {
		t.Fatalf("same quote_id %q", resp1.QuoteID)
	}
}

func TestGetQuote_returnsSameSnapshot(t *testing.T) {
	h := testHandler(t, nil, nil)
	body := exampleRequestBody(t, "01_source_percent_fee.json")
	postRec, postResp, _ := postJSON(t, h, body)
	if postRec.Code != http.StatusCreated {
		t.Fatalf("post status=%d body=%s", postRec.Code, postRec.Body.String())
	}
	getRec, getResp, _ := getQuote(t, h, postResp.QuoteID)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	if getResp.QuoteID != postResp.QuoteID {
		t.Fatalf("quote_id get=%q post=%q", getResp.QuoteID, postResp.QuoteID)
	}
	if getResp.DirectionID != postResp.DirectionID {
		t.Fatalf("direction_id mismatch")
	}
	if getResp.SourceAmount != postResp.SourceAmount {
		t.Fatalf("source_amount mismatch")
	}
	if getResp.Fee != postResp.Fee {
		t.Fatalf("fee mismatch")
	}
	if getResp.AmountToConvert != postResp.AmountToConvert {
		t.Fatalf("amount_to_convert mismatch")
	}
	if getResp.Rate != postResp.Rate {
		t.Fatalf("rate mismatch")
	}
	if getResp.TargetAmount != postResp.TargetAmount {
		t.Fatalf("target_amount mismatch")
	}
	if getResp.ExpiresAt != postResp.ExpiresAt {
		t.Fatalf("expires_at mismatch")
	}
	breakdownEqual(t, getResp.Breakdown, postResp.Breakdown)
}

func TestGetQuote_unknownID(t *testing.T) {
	h := testHandler(t, nil, nil)
	rec, _, errBody := getQuote(t, h, "missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if errBody.Error.Code != "QUOTE_NOT_FOUND" {
		t.Fatalf("code=%q", errBody.Error.Code)
	}
}
