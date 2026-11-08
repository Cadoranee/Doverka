package test

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

const parallelPostN = 32

var parallelPostFakeNow = time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

type parallelTransferResponse struct {
	TransferID string `json:"transfer_id"`
	Status     string `json:"status"`
}

type parallelPostResult struct {
	status int
	body   []byte
}

func parallelRandHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func parallelInsertQuote(t *testing.T, db *store.DB, quoteID string, expiresAt, createdAt time.Time) {
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
		int64(150000),
		"0.3700",
		expiresAt,
		createdAt,
	)
	if err != nil {
		t.Fatalf("insert quote: %v", err)
	}
}

func parallelSeedBalance(t *testing.T, db *store.DB, led *ledger.Ledger, clientID string, amount int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := led.Seed(ctx, tx, clientID, "RUB", amount); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func parallelCleanupClient(db *store.DB, clientID, quoteID, idempotencyKey string) {
	ctx := context.Background()
	pool := db.Pool()
	_, _ = pool.Exec(ctx, `DELETE FROM ledger_entries WHERE client_id = $1`, clientID)
	_, _ = pool.Exec(ctx, `DELETE FROM provider_attempts WHERE transfer_id IN (SELECT transfer_id FROM transfers WHERE client_id = $1)`, clientID)
	_, _ = pool.Exec(ctx, `DELETE FROM transfer_transitions WHERE transfer_id IN (SELECT transfer_id FROM transfers WHERE client_id = $1)`, clientID)
	_, _ = pool.Exec(ctx, `DELETE FROM transfers WHERE client_id = $1`, clientID)
	if quoteID != "" {
		_, _ = pool.Exec(ctx, `DELETE FROM quotes WHERE quote_id = $1`, quoteID)
	}
	if idempotencyKey != "" {
		_, _ = pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE idempotency_key = $1`, idempotencyKey)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM client_balances WHERE client_id = $1`, clientID)
}

func parallelCountTransfersByQuote(t *testing.T, db *store.DB, quoteID string) int {
	t.Helper()
	var n int
	err := db.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM transfers WHERE quote_id = $1`, quoteID).Scan(&n)
	if err != nil {
		t.Fatalf("count transfers by quote: %v", err)
	}
	return n
}

func parallelCountTransfersByClient(t *testing.T, db *store.DB, clientID string) int {
	t.Helper()
	var n int
	err := db.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM transfers WHERE client_id = $1`, clientID).Scan(&n)
	if err != nil {
		t.Fatalf("count transfers by client: %v", err)
	}
	return n
}

func parallelTransferIDInDB(t *testing.T, db *store.DB, quoteID string) string {
	t.Helper()
	var id string
	err := db.Pool().QueryRow(context.Background(), `SELECT transfer_id FROM transfers WHERE quote_id = $1`, quoteID).Scan(&id)
	if err != nil {
		t.Fatalf("select transfer_id: %v", err)
	}
	return id
}

func parallelPost(ctx context.Context, handler http.Handler, idempotencyKey string, body []byte) parallelPostResult {
	req := httptest.NewRequest(http.MethodPost, "http://example/transfers", bytes.NewReader(body))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return parallelPostResult{status: rec.Code, body: append([]byte(nil), rec.Body.Bytes()...)}
}

func TestParallelPostTransfer_sameIdempotencyKeyYieldsOneTransfer(t *testing.T) {
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
	quoteID := "quote-" + parallelRandHex(t, 8)
	idemKey := "parallel-" + parallelRandHex(t, 8)
	t.Cleanup(func() { parallelCleanupClient(db, clientID, quoteID, idemKey) })

	parallelInsertQuote(t, db, quoteID, parallelPostFakeNow.Add(time.Hour), parallelPostFakeNow)
	parallelSeedBalance(t, db, led, clientID, 10_000_000)

	body := []byte(fmt.Sprintf(`{"quote_id":%q,"client_id":%q,"recipient":{"account":"123"}}`, quoteID, clientID))

	postCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := make(chan struct{})
	results := make([]parallelPostResult, parallelPostN)
	var wg sync.WaitGroup
	for i := range parallelPostN {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = parallelPost(postCtx, handler, idemKey, body)
		}(i)
	}
	close(start)
	wg.Wait()

	ids := make(map[string]struct{})
	var wantStatus string
	for i, res := range results {
		if res.status != http.StatusAccepted {
			t.Errorf("goroutine %d: status=%d body=%s", i, res.status, string(res.body))
		}
		var parsed parallelTransferResponse
		if err := json.Unmarshal(res.body, &parsed); err != nil {
			t.Errorf("goroutine %d: decode: %v body=%s", i, err, string(res.body))
			continue
		}
		if parsed.TransferID == "" {
			t.Errorf("goroutine %d: empty transfer_id", i)
			continue
		}
		ids[parsed.TransferID] = struct{}{}
		switch parsed.Status {
		case "COMPLETED", "FAILED", "NEEDS_REVIEW":
			t.Errorf("goroutine %d: terminal status %q", i, parsed.Status)
		}
		if wantStatus == "" {
			wantStatus = parsed.Status
		} else if parsed.Status != wantStatus {
			t.Errorf("goroutine %d: status %q != %q", i, parsed.Status, wantStatus)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("distinct transfer_id: got %d (%v)", len(ids), ids)
	}
	var wantID string
	for id := range ids {
		wantID = id
	}

	if n := parallelCountTransfersByQuote(t, db, quoteID); n != 1 {
		t.Fatalf("transfers by quote_id: got %d want 1", n)
	}
	if n := parallelCountTransfersByClient(t, db, clientID); n != 1 {
		t.Fatalf("transfers by client_id: got %d want 1", n)
	}
	if dbID := parallelTransferIDInDB(t, db, quoteID); dbID != wantID {
		t.Fatalf("db transfer_id %q != http %q", dbID, wantID)
	}

	replay := parallelPost(postCtx, handler, idemKey, body)
	if replay.status != http.StatusAccepted {
		t.Fatalf("replay status=%d body=%s", replay.status, string(replay.body))
	}
	var replayParsed parallelTransferResponse
	if err := json.Unmarshal(replay.body, &replayParsed); err != nil {
		t.Fatalf("replay decode: %v", err)
	}
	if replayParsed.TransferID != wantID || replayParsed.Status != wantStatus {
		t.Fatalf("replay transfer_id=%q status=%q, want id=%q status=%q", replayParsed.TransferID, replayParsed.Status, wantID, wantStatus)
	}
}
