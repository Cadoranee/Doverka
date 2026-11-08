package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/httpapi"
	"github.com/Cadoranee/Doverka/internal/idempotency"
	"github.com/Cadoranee/Doverka/internal/ledger"
	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/Cadoranee/Doverka/internal/transfer"
)

type parallelBalanceAPIError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parallelInsertQuoteSourceAmount(t *testing.T, db *store.DB, quoteID string, sourceAmountMinor int64, expiresAt, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := db.Pool().Exec(ctx, `
INSERT INTO quotes (
	quote_id, direction_id, source_country, source_currency, target_country, target_currency,
	payout_method, amount_type, source_amount_minor, fee_amount_minor, fee_currency,
	amount_to_convert_minor, rate, target_amount_minor, breakdown, expires_at, created_at
) VALUES (
	$1, 'RU-TH-BANK', 'RU', 'RUB', 'TH', 'THB', 'BANK_ACCOUNT', 'SOURCE',
	$2, 1500, 'RUB', 148500, $3::numeric, 54945, '[]'::jsonb, $4, $5
)`,
		quoteID,
		sourceAmountMinor,
		"0.3700",
		expiresAt,
		createdAt,
	)
	if err != nil {
		t.Fatalf("insert quote: %v", err)
	}
}

func TestParallelPostTransfer_sameClientDepletingBalanceAtMostOneSucceeds(t *testing.T) {
	if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		t.Skip("DATABASE_URL is not set")
	}

	const sourceAmountMinor int64 = 150_000

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

	clk := clock.NewFake(parallelPostFakeNow)
	led := ledger.New(db.Pool())
	h := &httpapi.Handler{
		Store:       quote.NewRepo(db.Pool(), clk),
		Clock:       clk,
		DB:          db.Pool(),
		Transfers:   transfer.New(db.Pool(), clk),
		Ledger:      led,
		Idempotency: idempotency.New(db.Pool()),
	}
	handler := h.Routes()

	clientID := "client-" + parallelRandHex(t, 8)
	quoteID1 := "quote-" + parallelRandHex(t, 8)
	quoteID2 := "quote-" + parallelRandHex(t, 8)
	idemKey1 := "parallel-" + parallelRandHex(t, 8)
	idemKey2 := "parallel-" + parallelRandHex(t, 8)
	t.Cleanup(func() {
		parallelCleanupClient(db, clientID, quoteID1, idemKey1)
		parallelCleanupClient(db, clientID, quoteID2, idemKey2)
	})

	expires := parallelPostFakeNow.Add(time.Hour)
	parallelInsertQuoteSourceAmount(t, db, quoteID1, sourceAmountMinor, expires, parallelPostFakeNow)
	parallelInsertQuoteSourceAmount(t, db, quoteID2, sourceAmountMinor, expires, parallelPostFakeNow)
	parallelSeedBalance(t, db, led, clientID, sourceAmountMinor)

	body1 := []byte(fmt.Sprintf(`{"quote_id":%q,"client_id":%q,"recipient":{"account":"123"}}`, quoteID1, clientID))
	body2 := []byte(fmt.Sprintf(`{"quote_id":%q,"client_id":%q,"recipient":{"account":"123"}}`, quoteID2, clientID))

	postCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := make(chan struct{})
	results := make([]parallelPostResult, 2)
	bodies := [][]byte{body1, body2}
	keys := []string{idemKey1, idemKey2}
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = parallelPost(postCtx, handler, keys[i], bodies[i])
		}(i)
	}
	close(start)
	wg.Wait()

	var successCount, rejectCount int
	var successTransferID, successStatus string
	for i, res := range results {
		switch res.status {
		case http.StatusAccepted:
			successCount++
			var parsed parallelTransferResponse
			if err := json.Unmarshal(res.body, &parsed); err != nil {
				t.Fatalf("goroutine %d: decode success: %v body=%s", i, err, string(res.body))
			}
			if parsed.TransferID == "" {
				t.Fatalf("goroutine %d: empty transfer_id body=%s", i, string(res.body))
			}
			switch parsed.Status {
			case "COMPLETED", "FAILED", "NEEDS_REVIEW":
				t.Fatalf("goroutine %d: terminal status %q body=%s", i, parsed.Status, string(res.body))
			}
			if successTransferID != "" && successTransferID != parsed.TransferID {
				t.Fatalf("goroutine %d: multiple success transfer_ids", i)
			}
			successTransferID = parsed.TransferID
			successStatus = parsed.Status
		case http.StatusUnprocessableEntity:
			rejectCount++
			var parsed parallelBalanceAPIError
			if err := json.Unmarshal(res.body, &parsed); err != nil {
				t.Fatalf("goroutine %d: decode error: %v body=%s", i, err, string(res.body))
			}
			code := parsed.Error.Code
			if code != "INSUFFICIENT_FUNDS" && code != "DAILY_LIMIT_EXCEEDED" {
				t.Fatalf("goroutine %d: error.code=%q want INSUFFICIENT_FUNDS or DAILY_LIMIT_EXCEEDED body=%s", i, code, string(res.body))
			}
		default:
			t.Fatalf("goroutine %d: status=%d body=%s", i, res.status, string(res.body))
		}
	}
	if successCount != 1 {
		t.Fatalf("success count: got %d want 1", successCount)
	}
	if rejectCount != 1 {
		t.Fatalf("reject count: got %d want 1", rejectCount)
	}
	if successStatus == "" {
		t.Fatalf("missing success status")
	}

	if n := parallelCountTransfersByClient(t, db, clientID); n != 1 {
		t.Fatalf("transfers by client_id: got %d want 1", n)
	}
	var dbTransferID string
	err = db.Pool().QueryRow(ctx, `SELECT transfer_id FROM transfers WHERE client_id = $1`, clientID).Scan(&dbTransferID)
	if err != nil {
		t.Fatalf("select transfer_id by client: %v", err)
	}
	if dbTransferID != successTransferID {
		t.Fatalf("db transfer_id %q != http %q", dbTransferID, successTransferID)
	}

	bal, err := led.Balance(ctx, clientID, "RUB")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 0 {
		t.Fatalf("available: got %d want 0", bal.Available)
	}
	if bal.Reserved != sourceAmountMinor {
		t.Fatalf("reserved: got %d want %d", bal.Reserved, sourceAmountMinor)
	}
}
