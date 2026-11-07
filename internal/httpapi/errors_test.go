package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cadoranee/Doverka/internal/rates"
)

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, errBody apiErrorBody, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if errBody.Error.Code != wantCode {
		t.Fatalf("code=%q", errBody.Error.Code)
	}
}

func TestPostQuote_amountBelowMin(t *testing.T) {
	h := testHandler(t, nil, nil)
	body := exampleRequestBody(t, "04_below_min.json")
	rec, _, errBody := postJSON(t, h, body)
	assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "AMOUNT_BELOW_MIN")
}

func TestPostQuote_amountAboveMax(t *testing.T) {
	h := testHandler(t, nil, nil)
	body := exampleRequestBody(t, "05_above_max.json")
	rec, _, errBody := postJSON(t, h, body)
	assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "AMOUNT_ABOVE_MAX")
}

func TestPostQuote_targetSourceLimits(t *testing.T) {
	h := testHandler(t, nil, nil)
	request := func(amount string) []byte {
		return fmt.Appendf(nil, `{
  "source_country": "RU",
  "source_currency": "RUB",
  "target_country": "TH",
  "target_currency": "THB",
  "payout_method": "BANK_ACCOUNT",
  "amount": %q,
  "amount_type": "TARGET"
}`, amount)
	}
	t.Run("below_min", func(t *testing.T) {
		rec, _, errBody := postJSON(t, h, request("398.99"))
		assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "AMOUNT_BELOW_MIN")
	})
	t.Run("above_max", func(t *testing.T) {
		rec, _, errBody := postJSON(t, h, request("124110.01"))
		assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "AMOUNT_ABOVE_MAX")
	})
}

func TestPostQuote_unsupportedPayout(t *testing.T) {
	h := testHandler(t, nil, nil)
	body := exampleRequestBody(t, "06_unsupported_method.json")
	rec, _, errBody := postJSON(t, h, body)
	assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "UNSUPPORTED_DIRECTION")
}

func TestPostQuote_validationError(t *testing.T) {
	h := testHandler(t, nil, nil)
	t.Run("invalid_json", func(t *testing.T) {
		rec, _, errBody := postJSON(t, h, []byte("{"))
		assertAPIError(t, rec, errBody, http.StatusBadRequest, "VALIDATION_ERROR")
	})
	t.Run("bad_amount_scale", func(t *testing.T) {
		raw := `{
  "source_country": "RU",
  "source_currency": "RUB",
  "target_country": "TH",
  "target_currency": "THB",
  "payout_method": "BANK_ACCOUNT",
  "amount": "5000",
  "amount_type": "SOURCE"
}`
		rec, _, errBody := postJSON(t, h, []byte(raw))
		assertAPIError(t, rec, errBody, http.StatusBadRequest, "VALIDATION_ERROR")
	})
}

func TestPostQuote_rateUnavailable(t *testing.T) {
	store := &memStore{now: fixedNow}
	h := testHandler(t, store, rates.ErrUnavailable)
	body := exampleRequestBody(t, "01_source_percent_fee.json")
	rec, _, errBody := postJSON(t, h, body)
	assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "RATE_UNAVAILABLE")
	if store.len() != 0 {
		t.Fatalf("store saved %d quotes", store.len())
	}
}
