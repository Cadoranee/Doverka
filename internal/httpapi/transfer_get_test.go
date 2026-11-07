package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/transfer"
)

func TestGetTransfer_returnsStatusHistoryAndAttempts(t *testing.T) {
	db, h, led := openTransferHTTPTest(t)
	h.Catalog = loadCatalog(t)
	h.TransferView = transfer.New(db.Pool(), clock.NewFake(transferFixedNow))

	clientID := "client-" + randHexID(t, 8)
	quoteID := "quote-" + randHexID(t, 8)
	idemKey := "idem-" + randHexID(t, 8)
	t.Cleanup(func() { cleanupTransferClient(t, db, clientID, quoteID, idemKey) })
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.Pool().Exec(ctx, `
DELETE FROM provider_attempts WHERE transfer_id IN (SELECT transfer_id FROM transfers WHERE client_id = $1)`,
			clientID)
	})

	insertTransferTestQuote(t, db.Pool(), quoteID, 150000, transferFixedNow.Add(time.Hour))
	seedBalance(t, db, led, clientID, 500000)

	body := transferBody(quoteID, clientID, "")
	rec, created, _ := postTransfer(t, h.Routes(), idemKey, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status: got %d want %d", rec.Code, http.StatusAccepted)
	}
	if created.Status != "RESERVED" {
		t.Fatalf("POST body status: got %q want RESERVED", created.Status)
	}
	transferID := created.TransferID

	attemptAt := transferFixedNow.Add(time.Minute)
	attemptKey := "attempt-" + randHexID(t, 8)
	ctx := context.Background()
	_, err := db.Pool().Exec(ctx, `
INSERT INTO provider_attempts (transfer_id, provider_id, idempotency_key, payout_id, result, attempted_at)
VALUES ($1, 'bank-th', $2, 'po-77', 'accepted', $3)`,
		transferID, attemptKey, attemptAt,
	)
	if err != nil {
		t.Fatalf("insert provider attempt: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example/transfers/"+transferID, nil)
	getRec := httptest.NewRecorder()
	h.Routes().ServeHTTP(getRec, req)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status: got %d body=%s", getRec.Code, getRec.Body.String())
	}
	if ct := getRec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type: got %q", ct)
	}

	var detail transferDetailResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v body=%s", err, getRec.Body.String())
	}
	if detail.TransferID != transferID {
		t.Fatalf("transfer_id: got %q", detail.TransferID)
	}
	if detail.Status != "RESERVED" {
		t.Fatalf("status: got %q want RESERVED", detail.Status)
	}
	if detail.QuoteID != quoteID {
		t.Fatalf("quote_id: got %q", detail.QuoteID)
	}
	if detail.ClientID != clientID {
		t.Fatalf("client_id: got %q", detail.ClientID)
	}
	if detail.SourceAmount.Amount != "1500.00" || detail.SourceAmount.Currency != "RUB" {
		t.Fatalf("source_amount: got %+v", detail.SourceAmount)
	}
	if detail.TargetAmount.Amount != "549.45" || detail.TargetAmount.Currency != "THB" {
		t.Fatalf("target_amount: got %+v", detail.TargetAmount)
	}
	if len(detail.Transitions) != 2 {
		t.Fatalf("transitions len: got %d", len(detail.Transitions))
	}
	wantTransitions := []transferTransitionJSON{
		{From: "", To: "CREATED", Reason: "created", At: "2026-06-15T10:00:00Z"},
		{From: "CREATED", To: "RESERVED", Reason: "reserve", At: "2026-06-15T10:00:00Z"},
	}
	for i := range wantTransitions {
		if detail.Transitions[i] != wantTransitions[i] {
			t.Fatalf("transition[%d]: got %+v want %+v", i, detail.Transitions[i], wantTransitions[i])
		}
	}
	if len(detail.ProviderAttempts) != 1 {
		t.Fatalf("provider_attempts len: got %d", len(detail.ProviderAttempts))
	}
	wantAttempt := transferAttemptJSON{
		ProviderID:  "bank-th",
		AttemptedAt: "2026-06-15T10:01:00Z",
		Result:      "accepted",
		PayoutID:    "po-77",
	}
	if detail.ProviderAttempts[0] != wantAttempt {
		t.Fatalf("provider_attempt: got %+v want %+v", detail.ProviderAttempts[0], wantAttempt)
	}

	raw := getRec.Body.String()
	if !strings.Contains(raw, `"transitions":`) || !strings.Contains(raw, `"provider_attempts":`) {
		t.Fatalf("raw JSON missing keys: %s", raw)
	}
	if strings.Contains(raw, `"transitions":null`) || strings.Contains(raw, `"provider_attempts":null`) {
		t.Fatalf("raw JSON must not contain null slices: %s", raw)
	}
}

func TestGetTransfer_unknownID(t *testing.T) {
	db, h, _ := openTransferHTTPTest(t)
	h.Catalog = loadCatalog(t)
	h.TransferView = transfer.New(db.Pool(), clock.NewFake(transferFixedNow))

	id := "does-not-exist-" + randHexID(t, 8)
	req := httptest.NewRequest(http.MethodGet, "http://example/transfers/"+id, nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	var errBody apiErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	assertAPIError(t, rec, errBody, http.StatusNotFound, "TRANSFER_NOT_FOUND")
}
