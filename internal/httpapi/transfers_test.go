package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/idempotency"
	"github.com/Cadoranee/Doverka/internal/ledger"
	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type transferExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var transferFixedNow = time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

func openTransferHTTPTest(t *testing.T) (*store.DB, *Handler, *ledger.Ledger) {
	t.Helper()
	if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	clk := clock.NewFake(transferFixedNow)
	led := ledger.New(db.Pool())
	h := &Handler{
		Store:       quote.NewRepo(db.Pool(), clk),
		Clock:       clk,
		DB:          db.Pool(),
		Transfers:   transfer.New(db.Pool(), clk),
		Ledger:      led,
		Idempotency: idempotency.New(db.Pool()),
	}
	return db, h, led
}

func randHexID(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func insertTransferTestQuote(t *testing.T, exec transferExecer, quoteID string, sourceAmountMinor int64, expiresAt time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := exec.Exec(ctx, `
INSERT INTO quotes (
	quote_id, direction_id, source_country, source_currency, target_country, target_currency,
	payout_method, amount_type, source_amount_minor, fee_amount_minor, fee_currency,
	amount_to_convert_minor, rate, target_amount_minor, breakdown, expires_at, created_at
) VALUES (
	$1, 'RU-TH-BANK', 'RU', 'RUB', 'TH', 'THB', 'BANK_ACCOUNT', 'SOURCE',
	$2, 1500, 'RUB', 148500, $3::numeric, 54945, '[]'::jsonb, $4, $4
)`,
		quoteID,
		sourceAmountMinor,
		"0.3700",
		expiresAt,
	)
	if err != nil {
		t.Fatalf("insert quote: %v", err)
	}
}

func transferBody(quoteID, clientID string, recipientExtra string) []byte {
	if recipientExtra == "" {
		recipientExtra = `"account":"123"`
	}
	return []byte(fmt.Sprintf(`{"quote_id":%q,"client_id":%q,"recipient":{%s}}`, quoteID, clientID, recipientExtra))
}

func postTransfer(t *testing.T, h http.Handler, idempotencyKey string, body []byte) (*httptest.ResponseRecorder, createTransferResponse, apiErrorBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://example/transfers", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		var errBody apiErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
			t.Fatalf("decode error body: %v body=%s", err, rec.Body.String())
		}
		return rec, createTransferResponse{}, errBody
	}
	var ok createTransferResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode success body: %v body=%s", err, rec.Body.String())
	}
	return rec, ok, apiErrorBody{}
}

func seedBalance(t *testing.T, db *store.DB, l *ledger.Ledger, clientID string, amount int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := l.Seed(ctx, tx, clientID, "RUB", amount); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func countTransfersByClient(t *testing.T, db *store.DB, clientID string) int {
	t.Helper()
	var n int
	err := db.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM transfers WHERE client_id = $1`, clientID).Scan(&n)
	if err != nil {
		t.Fatalf("count transfers: %v", err)
	}
	return n
}

func countTransfersByQuote(t *testing.T, db *store.DB, quoteID string) int {
	t.Helper()
	var n int
	err := db.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM transfers WHERE quote_id = $1`, quoteID).Scan(&n)
	if err != nil {
		t.Fatalf("count transfers by quote: %v", err)
	}
	return n
}

func transferStatus(t *testing.T, db *store.DB, transferID string) string {
	t.Helper()
	var status string
	err := db.Pool().QueryRow(context.Background(), `SELECT status FROM transfers WHERE transfer_id = $1`, transferID).Scan(&status)
	if err != nil {
		t.Fatalf("transfer status: %v", err)
	}
	return status
}

func cleanupTransferClient(t *testing.T, db *store.DB, clientID, quoteID, idempotencyKey string) {
	t.Helper()
	ctx := context.Background()
	_, _ = db.Pool().Exec(ctx, `DELETE FROM ledger_entries WHERE client_id = $1`, clientID)
	_, _ = db.Pool().Exec(ctx, `
DELETE FROM transfer_transitions WHERE transfer_id IN (SELECT transfer_id FROM transfers WHERE client_id = $1)`,
		clientID)
	_, _ = db.Pool().Exec(ctx, `DELETE FROM transfers WHERE client_id = $1`, clientID)
	if quoteID != "" {
		_, _ = db.Pool().Exec(ctx, `DELETE FROM quotes WHERE quote_id = $1`, quoteID)
	}
	if idempotencyKey != "" {
		_, _ = db.Pool().Exec(ctx, `DELETE FROM idempotency_keys WHERE idempotency_key = $1`, idempotencyKey)
	}
	_, _ = db.Pool().Exec(ctx, `DELETE FROM client_balances WHERE client_id = $1`, clientID)
}

func TestPostTransfer_missingIdempotencyKey(t *testing.T) {
	db, h, _ := openTransferHTTPTest(t)
	clientID := "client-" + randHexID(t, 8)
	quoteID := "quote-" + randHexID(t, 8)
	t.Cleanup(func() { cleanupTransferClient(t, db, clientID, quoteID, "") })
	insertTransferTestQuote(t, db.Pool(), quoteID, 150000, transferFixedNow.Add(time.Hour))
	body := transferBody(quoteID, clientID, "")
	rec, _, errBody := postTransfer(t, h.Routes(), "", body)
	assertAPIError(t, rec, errBody, http.StatusBadRequest, "VALIDATION_ERROR")
	if countTransfersByClient(t, db, clientID) != 0 {
		t.Fatalf("expected no transfers")
	}
}

func TestPostTransfer_unknownQuote(t *testing.T) {
	db, h, _ := openTransferHTTPTest(t)
	clientID := "client-" + randHexID(t, 8)
	quoteID := "quote-missing-" + randHexID(t, 8)
	idemKey := "idem-" + randHexID(t, 8)
	t.Cleanup(func() { cleanupTransferClient(t, db, clientID, "", idemKey) })
	body := transferBody(quoteID, clientID, "")
	rec, _, errBody := postTransfer(t, h.Routes(), idemKey, body)
	assertAPIError(t, rec, errBody, http.StatusNotFound, "QUOTE_NOT_FOUND")
	if countTransfersByQuote(t, db, quoteID) != 0 {
		t.Fatalf("expected no transfers for quote")
	}
}

func TestPostTransfer_expiredQuote(t *testing.T) {
	db, h, led := openTransferHTTPTest(t)
	clientID := "client-" + randHexID(t, 8)
	quoteID := "quote-exp-" + randHexID(t, 8)
	idemKey := "idem-" + randHexID(t, 8)
	t.Cleanup(func() { cleanupTransferClient(t, db, clientID, quoteID, idemKey) })
	insertTransferTestQuote(t, db.Pool(), quoteID, 150000, transferFixedNow)
	seedBalance(t, db, led, clientID, 150000)
	body := transferBody(quoteID, clientID, "")
	rec, _, errBody := postTransfer(t, h.Routes(), idemKey, body)
	assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "QUOTE_EXPIRED")
	if countTransfersByQuote(t, db, quoteID) != 0 {
		t.Fatalf("expected no transfers for expired quote")
	}
	bal, err := led.Balance(context.Background(), clientID, "RUB")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Available != 150000 || bal.Reserved != 0 {
		t.Fatalf("available=%d reserved=%d", bal.Available, bal.Reserved)
	}
}

func TestPostTransfer_insufficientFunds(t *testing.T) {
	db, h, led := openTransferHTTPTest(t)
	const sourceAmount int64 = 150000
	clientID := "client-" + randHexID(t, 8)
	quoteID := "quote-funds-" + randHexID(t, 8)
	idemKey := "idem-" + randHexID(t, 8)
	t.Cleanup(func() { cleanupTransferClient(t, db, clientID, quoteID, idemKey) })
	insertTransferTestQuote(t, db.Pool(), quoteID, sourceAmount, transferFixedNow.Add(time.Hour))
	seedBalance(t, db, led, clientID, sourceAmount-1)
	body := transferBody(quoteID, clientID, "")
	rec, _, errBody := postTransfer(t, h.Routes(), idemKey, body)
	assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS")
	if countTransfersByQuote(t, db, quoteID) != 0 {
		t.Fatalf("expected no transfer row")
	}
	bal, err := led.Balance(context.Background(), clientID, "RUB")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Available != sourceAmount-1 || bal.Reserved != 0 {
		t.Fatalf("available=%d reserved=%d", bal.Available, bal.Reserved)
	}
	rec2, _, errBody2 := postTransfer(t, h.Routes(), idemKey, body)
	assertAPIError(t, rec2, errBody2, http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS")
	if countTransfersByQuote(t, db, quoteID) != 0 {
		t.Fatalf("replay created a transfer")
	}
}

func TestPostTransfer_noBalanceRow(t *testing.T) {
	db, h, _ := openTransferHTTPTest(t)
	const sourceAmount int64 = 150000
	clientID := "client-" + randHexID(t, 8)
	quoteID := "quote-empty-" + randHexID(t, 8)
	idemKey := "idem-" + randHexID(t, 8)
	t.Cleanup(func() { cleanupTransferClient(t, db, clientID, quoteID, idemKey) })
	insertTransferTestQuote(t, db.Pool(), quoteID, sourceAmount, transferFixedNow.Add(time.Hour))
	body := transferBody(quoteID, clientID, "")
	rec, _, errBody := postTransfer(t, h.Routes(), idemKey, body)
	assertAPIError(t, rec, errBody, http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS")
	if countTransfersByQuote(t, db, quoteID) != 0 {
		t.Fatalf("expected no transfer row")
	}
}

func TestPostTransfer_successAndIdempotency(t *testing.T) {
	db, h, led := openTransferHTTPTest(t)
	const sourceAmount int64 = 150000
	clientID := "client-" + randHexID(t, 8)
	quoteID := "quote-ok-" + randHexID(t, 8)
	idemKey := "idem-" + randHexID(t, 8)
	t.Cleanup(func() { cleanupTransferClient(t, db, clientID, quoteID, idemKey) })
	insertTransferTestQuote(t, db.Pool(), quoteID, sourceAmount, transferFixedNow.Add(time.Hour))
	seedBalance(t, db, led, clientID, sourceAmount)
	body := transferBody(quoteID, clientID, `"bank":"SCB"`)
	rec, created, errBody := postTransfer(t, h.Routes(), idemKey, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d err=%+v", rec.Code, errBody)
	}
	if created.TransferID == "" {
		t.Fatalf("empty transfer_id")
	}
	if created.Status != "RESERVED" {
		t.Fatalf("status=%q", created.Status)
	}
	if transferStatus(t, db, created.TransferID) != "RESERVED" {
		t.Fatalf("db status not RESERVED")
	}
	bal, err := led.Balance(context.Background(), clientID, "RUB")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Available != 0 || bal.Reserved != sourceAmount {
		t.Fatalf("available=%d reserved=%d", bal.Available, bal.Reserved)
	}
	rec2, created2, _ := postTransfer(t, h.Routes(), idemKey, body)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("replay status=%d", rec2.Code)
	}
	if created2.TransferID != created.TransferID || created2.Status != "RESERVED" {
		t.Fatalf("replay id=%q status=%q", created2.TransferID, created2.Status)
	}
	if countTransfersByQuote(t, db, quoteID) != 1 {
		t.Fatalf("expected one transfer")
	}
	bal2, err := led.Balance(context.Background(), clientID, "RUB")
	if err != nil {
		t.Fatalf("balance after replay: %v", err)
	}
	if bal2.Reserved != sourceAmount {
		t.Fatalf("reserved doubled: %d", bal2.Reserved)
	}
}

func TestPostTransfer_idempotencyKeyReused(t *testing.T) {
	db, h, led := openTransferHTTPTest(t)
	const sourceAmount int64 = 150000
	clientID := "client-" + randHexID(t, 8)
	quoteID := "quote-reuse-" + randHexID(t, 8)
	idemKey := "idem-" + randHexID(t, 8)
	t.Cleanup(func() { cleanupTransferClient(t, db, clientID, quoteID, idemKey) })
	insertTransferTestQuote(t, db.Pool(), quoteID, sourceAmount, transferFixedNow.Add(time.Hour))
	seedBalance(t, db, led, clientID, sourceAmount)
	body := transferBody(quoteID, clientID, `"bank":"SCB"`)
	recOK, created, errOK := postTransfer(t, h.Routes(), idemKey, body)
	if recOK.Code != http.StatusAccepted || created.TransferID == "" {
		t.Fatalf("first status=%d id=%q err=%+v", recOK.Code, created.TransferID, errOK)
	}
	body2 := transferBody(quoteID, clientID, `"bank":"KBANK"`)
	rec, _, errBody := postTransfer(t, h.Routes(), idemKey, body2)
	assertAPIError(t, rec, errBody, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED")
	if countTransfersByQuote(t, db, quoteID) != 1 {
		t.Fatalf("expected one transfer")
	}
	if countTransfersByClient(t, db, clientID) != 1 {
		t.Fatalf("expected one transfer for client")
	}
	if transferStatus(t, db, created.TransferID) != "RESERVED" {
		t.Fatalf("original transfer changed")
	}
}
