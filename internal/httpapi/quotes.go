package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Cadoranee/Doverka/internal/calc"
	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/rates"
	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

type Handler struct {
	Catalog  *refdata.Catalog
	Balances BalanceLister
	Rates    interface {
		Rate(ctx context.Context, base, quote string) (decimal.Decimal, error)
	}
	Store interface {
		Save(ctx context.Context, snap quote.Snapshot, ttl time.Duration) (quote.Quote, error)
		Get(ctx context.Context, quoteID string) (quote.Quote, error)
	}
	Clock       clockNow
	DB          txBeginner
	Transfers   transferCreator
	Ledger      balanceReserver
	Idempotency idempotencyRecorder
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /quotes", h.serveCreateQuote)
	mux.HandleFunc("GET /quotes/{quote_id}", h.serveGetQuote)
	mux.HandleFunc("GET /clients/{client_id}/balance", h.serveClientBalance)
	mux.HandleFunc("POST /transfers", h.serveCreateTransfer)
	return mux
}

type quoteRequest struct {
	SourceCountry  string `json:"source_country"`
	SourceCurrency string `json:"source_currency"`
	TargetCountry  string `json:"target_country"`
	TargetCurrency string `json:"target_currency"`
	PayoutMethod   string `json:"payout_method"`
	Amount         string `json:"amount"`
	AmountType     string `json:"amount_type"`
}

type moneyResponse struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type quoteResponse struct {
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

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *Handler) serveCreateQuote(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Catalog == nil || h.Rates == nil || h.Store == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	var req quoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "VALIDATION_ERROR")
		return
	}
	resolved, err := quote.Resolve(h.Catalog, quote.Input{
		SourceCountry:  req.SourceCountry,
		SourceCurrency: req.SourceCurrency,
		TargetCountry:  req.TargetCountry,
		TargetCurrency: req.TargetCurrency,
		PayoutMethod:   req.PayoutMethod,
		Amount:         req.Amount,
		AmountType:     req.AmountType,
	})
	if err != nil {
		var qe *quote.Error
		if errors.As(err, &qe) {
			writeError(w, quoteErrorStatus(qe.Code), string(qe.Code), string(qe.Code))
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	tariff := resolved.Tariff
	rate, err := h.Rates.Rate(r.Context(), tariff.Source.Currency, tariff.Target.Currency)
	if err != nil {
		if errors.Is(err, rates.ErrUnavailable) {
			writeError(w, http.StatusUnprocessableEntity, "RATE_UNAVAILABLE", "RATE_UNAVAILABLE")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	sourceCur, ok := h.Catalog.Currency(tariff.Source.Currency)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	targetCur, ok := h.Catalog.Currency(tariff.Target.Currency)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	var fwd calc.ForwardQuote
	switch req.AmountType {
	case "SOURCE":
		fwd, err = calc.Forward(resolved.Amount, rate, tariff.Fee, sourceCur, targetCur)
	case "TARGET":
		fwd, err = calc.Inverse(resolved.Amount, rate, tariff.Fee, sourceCur, targetCur)
		if err == nil {
			if limErr := quote.CheckSourceLimits(tariff, fwd.SourceAmount.Amount); limErr != nil {
				var qe *quote.Error
				if errors.As(limErr, &qe) {
					writeError(w, quoteErrorStatus(qe.Code), string(qe.Code), string(qe.Code))
					return
				}
				writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
				return
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "VALIDATION_ERROR")
		return
	}
	if err != nil {
		var qe *quote.Error
		if errors.As(err, &qe) {
			writeError(w, quoteErrorStatus(qe.Code), string(qe.Code), string(qe.Code))
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	feeCur, ok := h.Catalog.Currency(fwd.Fee.Currency)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	snap, err := buildSnapshot(tariff, req.AmountType, rate, fwd, sourceCur, feeCur, targetCur)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	ttl := time.Duration(tariff.QuoteTTLSec) * time.Second
	saved, err := h.Store.Save(r.Context(), snap, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	writeQuote(w, http.StatusCreated, h.Catalog, saved)
}

func (h *Handler) serveGetQuote(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Catalog == nil || h.Store == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	id := r.PathValue("quote_id")
	if id == "" {
		writeError(w, http.StatusNotFound, "QUOTE_NOT_FOUND", "QUOTE_NOT_FOUND")
		return
	}
	q, err := h.Store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, quote.ErrNotFound) {
			writeError(w, http.StatusNotFound, "QUOTE_NOT_FOUND", "QUOTE_NOT_FOUND")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	writeQuote(w, http.StatusOK, h.Catalog, q)
}

func buildSnapshot(
	tariff refdata.Tariff,
	amountType string,
	rate decimal.Decimal,
	fwd calc.ForwardQuote,
	sourceCur, feeCur, targetCur refdata.Currency,
) (quote.Snapshot, error) {
	sourceMinor, err := amountToMinor(fwd.SourceAmount.Amount, sourceCur.MinorUnits)
	if err != nil {
		return quote.Snapshot{}, err
	}
	feeMinor, err := amountToMinor(fwd.Fee.Amount, feeCur.MinorUnits)
	if err != nil {
		return quote.Snapshot{}, err
	}
	convertMinor, err := amountToMinor(fwd.AmountToConvert.Amount, sourceCur.MinorUnits)
	if err != nil {
		return quote.Snapshot{}, err
	}
	targetMinor, err := amountToMinor(fwd.TargetAmount.Amount, targetCur.MinorUnits)
	if err != nil {
		return quote.Snapshot{}, err
	}
	steps := make([]map[string]string, len(fwd.Breakdown))
	for i, line := range fwd.Breakdown {
		steps[i] = map[string]string{"step": line}
	}
	breakdown, err := json.Marshal(steps)
	if err != nil {
		return quote.Snapshot{}, err
	}
	return quote.Snapshot{
		DirectionID:          tariff.DirectionID,
		SourceCountry:        tariff.Source.Country,
		SourceCurrency:       tariff.Source.Currency,
		TargetCountry:        tariff.Target.Country,
		TargetCurrency:       tariff.Target.Currency,
		PayoutMethod:         tariff.PayoutMethod,
		AmountType:           amountType,
		SourceAmountMinor:    sourceMinor,
		FeeAmountMinor:       feeMinor,
		FeeCurrency:          fwd.Fee.Currency,
		AmountToConvertMinor: convertMinor,
		Rate:                 rate,
		TargetAmountMinor:    targetMinor,
		Breakdown:            breakdown,
	}, nil
}

func amountToMinor(amount decimal.Decimal, minorUnits int) (int64, error) {
	scaled := amount.Shift(int32(minorUnits))
	if !scaled.Equal(scaled.Truncate(0)) {
		return 0, fmt.Errorf("amount is not whole minor units")
	}
	return scaled.IntPart(), nil
}

func quoteToResponse(q quote.Quote, cat *refdata.Catalog) (quoteResponse, error) {
	sourceCur, ok := cat.Currency(q.SourceCurrency)
	if !ok {
		return quoteResponse{}, fmt.Errorf("unknown source currency")
	}
	feeCur, ok := cat.Currency(q.FeeCurrency)
	if !ok {
		return quoteResponse{}, fmt.Errorf("unknown fee currency")
	}
	targetCur, ok := cat.Currency(q.TargetCurrency)
	if !ok {
		return quoteResponse{}, fmt.Errorf("unknown target currency")
	}
	resp := quoteResponse{
		QuoteID:         q.ID,
		DirectionID:     q.DirectionID,
		SourceAmount:    minorToMoney(q.SourceAmountMinor, sourceCur.MinorUnits, q.SourceCurrency),
		Fee:             minorToMoney(q.FeeAmountMinor, feeCur.MinorUnits, q.FeeCurrency),
		AmountToConvert: minorToMoney(q.AmountToConvertMinor, sourceCur.MinorUnits, q.SourceCurrency),
		Rate:            formatRate(q.Rate),
		TargetAmount:    minorToMoney(q.TargetAmountMinor, targetCur.MinorUnits, q.TargetCurrency),
		Breakdown:       append(json.RawMessage(nil), q.Breakdown...),
		ExpiresAt:       q.ExpiresAt.UTC().Format(time.RFC3339),
	}
	return resp, nil
}

func minorToMoney(minor int64, scale int, currency string) moneyResponse {
	d := decimal.New(minor, -int32(scale))
	return moneyResponse{
		Amount:   d.StringFixed(int32(scale)),
		Currency: currency,
	}
}

func formatRate(rate decimal.Decimal) string {
	exp := rate.Exponent()
	if exp >= 0 {
		return rate.StringFixed(0)
	}
	return rate.StringFixed(-exp)
}

func quoteErrorStatus(code quote.Code) int {
	switch code {
	case quote.CodeValidationError:
		return http.StatusBadRequest
	case quote.CodeUnsupportedDirection, quote.CodeAmountBelowMin, quote.CodeAmountAboveMax:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func writeQuote(w http.ResponseWriter, status int, cat *refdata.Catalog, q quote.Quote) {
	resp, err := quoteToResponse(q, cat)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	writeJSON(w, status, resp)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorResponse
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
