package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/ledger"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const testWebhookSecret = "test-webhook-secret"

type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

type processingFixture struct {
	db         *store.DB
	m          *transfer.Machine
	led        *ledger.Ledger
	fake       *clock.Fake
	h          *Handler
	clientID   string
	transferID string
	quoteID    string
	providerID string
	payoutID   string
}

func openWebhookTest(t *testing.T) (*store.DB, *transfer.Machine, *ledger.Ledger, *clock.Fake) {
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
	fake := clock.NewFake(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC))
	return db, transfer.New(db.Pool(), fake), ledger.New(db.Pool()), fake
}

func newTestID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func insertQuote(t *testing.T, exec execer, quoteID string) {
	t.Helper()
	ctx := context.Background()
	expires := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	_, err := exec.Exec(ctx, `
INSERT INTO quotes (
	quote_id, direction_id, source_country, source_currency, target_country, target_currency,
	payout_method, amount_type, source_amount_minor, fee_amount_minor, fee_currency,
	amount_to_convert_minor, rate, target_amount_minor, breakdown, expires_at, created_at
) VALUES (
	$1, 'RU-TH-BANK', 'RU', 'RUB', 'TH', 'THB', 'BANK_ACCOUNT', 'SOURCE',
	150000, 1500, 'RUB', 148500, $2::numeric, 54945, '[]'::jsonb, $3, $3
)`,
		quoteID,
		"0.3700",
		expires,
	)
	if err != nil {
		t.Fatalf("insert quote: %v", err)
	}
}

func inTx(t *testing.T, db *store.DB, fn func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func cleanupWebhookFixture(t *testing.T, f *processingFixture) {
	t.Helper()
	ctx := context.Background()
	pool := f.db.Pool()
	if _, err := pool.Exec(ctx, `DELETE FROM webhook_events WHERE provider_id = $1`, f.providerID); err != nil {
		t.Fatalf("delete webhook_events: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM provider_attempts WHERE transfer_id = $1`, f.transferID); err != nil {
		t.Fatalf("delete provider_attempts: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM transfer_transitions WHERE transfer_id = $1`, f.transferID); err != nil {
		t.Fatalf("delete transitions: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries WHERE client_id = $1 OR transfer_id = $2`, f.clientID, f.transferID); err != nil {
		t.Fatalf("delete ledger_entries: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM transfers WHERE transfer_id = $1`, f.transferID); err != nil {
		t.Fatalf("delete transfers: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM quotes WHERE quote_id = $1`, f.quoteID); err != nil {
		t.Fatalf("delete quotes: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM client_balances WHERE client_id = $1`, f.clientID); err != nil {
		t.Fatalf("delete balances: %v", err)
	}
}

func newProcessingFixture(t *testing.T) *processingFixture {
	t.Helper()
	db, m, led, fake := openWebhookTest(t)
	ctx := context.Background()
	clientID := "c_" + newTestID(t)
	quoteID := newTestID(t)
	transferID := newTestID(t)
	providerID := "prov_" + newTestID(t)
	payoutID := "po_" + newTestID(t)
	insertQuote(t, db.Pool(), quoteID)
	nt := transfer.NewTransfer{
		ID:                transferID,
		QuoteID:           quoteID,
		ClientID:          clientID,
		SourceAmountMinor: 150000,
		SourceCurrency:    "RUB",
		TargetAmountMinor: 54945,
		TargetCurrency:    "THB",
		Recipient:         json.RawMessage(`{"account":"TH1234"}`),
	}
	inTx(t, db, func(tx pgx.Tx) error {
		if err := led.Seed(ctx, tx, clientID, "RUB", 1_000_000); err != nil {
			return err
		}
		if _, err := m.Create(ctx, tx, nt); err != nil {
			return err
		}
		if err := led.Reserve(ctx, tx, clientID, "RUB", transferID, 150000); err != nil {
			return err
		}
		if _, err := m.Transition(ctx, tx, transferID, transfer.StatusReserved, "hold"); err != nil {
			return err
		}
		if _, err := m.Transition(ctx, tx, transferID, transfer.StatusSubmitting, "submit"); err != nil {
			return err
		}
		if _, err := m.Transition(ctx, tx, transferID, transfer.StatusProcessing, "payout created"); err != nil {
			return err
		}
		idemKey := newTestID(t)
		_, err := tx.Exec(ctx, `
INSERT INTO provider_attempts (transfer_id, provider_id, idempotency_key, payout_id, result, attempted_at)
VALUES ($1, $2, $3, $4, 'PROCESSING', $5)`,
			transferID, providerID, idemKey, payoutID, fake.Now().UTC(),
		)
		return err
	})
	f := &processingFixture{
		db: db, m: m, led: led, fake: fake,
		clientID: clientID, transferID: transferID, quoteID: quoteID,
		providerID: providerID, payoutID: payoutID,
	}
	f.h = &Handler{
		Secret:    testWebhookSecret,
		Pool:      db.Pool(),
		Transfers: m,
		Ledger:    led,
		Clock:     fake,
	}
	t.Cleanup(func() { cleanupWebhookFixture(t, f) })
	return f
}

type webhookBody struct {
	EventID    string `json:"event_id"`
	PayoutID   string `json:"payout_id"`
	Reference  string `json:"reference"`
	Status     string `json:"status"`
	OccurredAt string `json:"occurred_at"`
}

func signBody(secret string, raw []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, h http.Handler, providerID string, body webhookBody, secret string, signature string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	sig := signature
	if sig == "" && secret != "" {
		sig = signBody(secret, raw)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+providerID, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Signature", sig)
	}
	req.SetPathValue("provider_id", providerID)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func assertHTTP200(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d want 200", rr.Code)
	}
}

func assertBalance(t *testing.T, led *ledger.Ledger, clientID string, available, reserved int64) {
	t.Helper()
	b, err := led.Balance(context.Background(), clientID, "RUB")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if b.Available != available || b.Reserved != reserved {
		t.Fatalf("balance available=%d reserved=%d want %d/%d", b.Available, b.Reserved, available, reserved)
	}
}

func countWebhookEvents(t *testing.T, db *store.DB, providerID, eventID string) int {
	t.Helper()
	var n int
	err := db.Pool().QueryRow(context.Background(), `
SELECT COUNT(*) FROM webhook_events WHERE provider_id = $1 AND event_id = $2`,
		providerID, eventID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count webhook_events: %v", err)
	}
	return n
}

func countTransitionsTo(t *testing.T, m *transfer.Machine, transferID string, to transfer.Status) int {
	t.Helper()
	hist, err := m.History(context.Background(), transferID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	n := 0
	for _, tr := range hist {
		if tr.To == to {
			n++
		}
	}
	return n
}

func TestWebhookDuplicateEventSingleTransition(t *testing.T) {
	f := newProcessingFixture(t)
	eventID := "ev_" + newTestID(t)
	body := webhookBody{
		EventID:    eventID,
		PayoutID:   f.payoutID,
		Reference:  f.transferID,
		Status:     eventStatusCompleted,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	h := f.h.Handler()
	rr1 := postWebhook(t, h, f.providerID, body, testWebhookSecret, "")
	rr2 := postWebhook(t, h, f.providerID, body, testWebhookSecret, "")
	assertHTTP200(t, rr1)
	assertHTTP200(t, rr2)
	tr, err := f.m.Get(context.Background(), f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED", tr.Status)
	}
	if n := countTransitionsTo(t, f.m, f.transferID, transfer.StatusCompleted); n != 1 {
		t.Fatalf("COMPLETED transitions %d want 1", n)
	}
	assertBalance(t, f.led, f.clientID, 850000, 0)
	if n := countWebhookEvents(t, f.db, f.providerID, eventID); n != 1 {
		t.Fatalf("webhook_events count %d want 1", n)
	}
}

func TestWebhookInvalidSignatureNoStateChange(t *testing.T) {
	f := newProcessingFixture(t)
	eventID := "ev_" + newTestID(t)
	body := webhookBody{
		EventID:    eventID,
		PayoutID:   f.payoutID,
		Reference:  f.transferID,
		Status:     eventStatusCompleted,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	h := f.h.Handler()
	rrMissing := postWebhook(t, h, f.providerID, body, "", "")
	assertHTTP200(t, rrMissing)
	rrWrong := postWebhook(t, h, f.providerID, body, "wrong-secret", "")
	assertHTTP200(t, rrWrong)
	tr, err := f.m.Get(context.Background(), f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusProcessing {
		t.Fatalf("status %s want PROCESSING", tr.Status)
	}
	if n := countWebhookEvents(t, f.db, f.providerID, eventID); n != 0 {
		t.Fatalf("webhook_events count %d want 0", n)
	}
	assertBalance(t, f.led, f.clientID, 850000, 150000)
}

func TestWebhookUnknownPayoutNoPanic(t *testing.T) {
	f := newProcessingFixture(t)
	h := f.h.Handler()
	unknownRef := newTestID(t)
	body1 := webhookBody{
		EventID:    "ev_" + newTestID(t),
		PayoutID:   "po_" + newTestID(t),
		Reference:  unknownRef,
		Status:     eventStatusCompleted,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	rr1 := postWebhook(t, h, f.providerID, body1, testWebhookSecret, "")
	assertHTTP200(t, rr1)
	body2 := webhookBody{
		EventID:    "ev_" + newTestID(t),
		PayoutID:   "po_" + newTestID(t),
		Reference:  f.transferID,
		Status:     eventStatusCompleted,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	rr2 := postWebhook(t, h, f.providerID, body2, testWebhookSecret, "")
	assertHTTP200(t, rr2)
	tr, err := f.m.Get(context.Background(), f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusProcessing {
		t.Fatalf("status %s want PROCESSING", tr.Status)
	}
	assertBalance(t, f.led, f.clientID, 850000, 150000)
}

func TestWebhookTerminalCompletedIgnoresContradiction(t *testing.T) {
	f := newProcessingFixture(t)
	h := f.h.Handler()
	first := webhookBody{
		EventID:    "ev_" + newTestID(t),
		PayoutID:   f.payoutID,
		Reference:  f.transferID,
		Status:     eventStatusCompleted,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	assertHTTP200(t, postWebhook(t, h, f.providerID, first, testWebhookSecret, ""))
	tr, err := f.m.Get(context.Background(), f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED after first webhook", tr.Status)
	}
	assertBalance(t, f.led, f.clientID, 850000, 0)
	failed := webhookBody{
		EventID:    "ev_" + newTestID(t),
		PayoutID:   f.payoutID,
		Reference:  f.transferID,
		Status:     eventStatusFailed,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	assertHTTP200(t, postWebhook(t, h, f.providerID, failed, testWebhookSecret, ""))
	tr, err = f.m.Get(context.Background(), f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED after FAILED event", tr.Status)
	}
	if n := countTransitionsTo(t, f.m, f.transferID, transfer.StatusFailed); n != 0 {
		t.Fatalf("FAILED transitions %d want 0", n)
	}
	assertBalance(t, f.led, f.clientID, 850000, 0)
	late := webhookBody{
		EventID:    "ev_" + newTestID(t),
		PayoutID:   f.payoutID,
		Reference:  f.transferID,
		Status:     eventStatusProcessing,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	assertHTTP200(t, postWebhook(t, h, f.providerID, late, testWebhookSecret, ""))
	tr, err = f.m.Get(context.Background(), f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED after PROCESSING event", tr.Status)
	}
	if n := countTransitionsTo(t, f.m, f.transferID, transfer.StatusCompleted); n != 1 {
		t.Fatalf("COMPLETED transitions %d want 1", n)
	}
}

func TestWebhookUnmatchedPayoutDoesNotConsumeEvent(t *testing.T) {
	f := newProcessingFixture(t)
	ctx := context.Background()
	if _, err := f.db.Pool().Exec(ctx, `DELETE FROM provider_attempts WHERE transfer_id = $1`, f.transferID); err != nil {
		t.Fatalf("delete attempt: %v", err)
	}
	eventID := "ev_" + newTestID(t)
	body := webhookBody{
		EventID:    eventID,
		PayoutID:   f.payoutID,
		Reference:  f.transferID,
		Status:     eventStatusCompleted,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	h := f.h.Handler()
	assertHTTP200(t, postWebhook(t, h, f.providerID, body, testWebhookSecret, ""))
	tr, err := f.m.Get(ctx, f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusProcessing {
		t.Fatalf("status %s want PROCESSING", tr.Status)
	}
	if n := countWebhookEvents(t, f.db, f.providerID, eventID); n != 0 {
		t.Fatalf("webhook_events count %d want 0", n)
	}
	assertBalance(t, f.led, f.clientID, 850000, 150000)
	inTx(t, f.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
INSERT INTO provider_attempts (transfer_id, provider_id, idempotency_key, payout_id, result, attempted_at)
VALUES ($1, $2, $3, $4, 'PROCESSING', $5)`,
			f.transferID, f.providerID, newTestID(t), f.payoutID, f.fake.Now().UTC(),
		)
		return err
	})
	assertHTTP200(t, postWebhook(t, h, f.providerID, body, testWebhookSecret, ""))
	tr, err = f.m.Get(ctx, f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED", tr.Status)
	}
	assertBalance(t, f.led, f.clientID, 850000, 0)
	if n := countWebhookEvents(t, f.db, f.providerID, eventID); n != 1 {
		t.Fatalf("webhook_events count %d want 1", n)
	}
}

func TestWebhookFailedReleasesReserve(t *testing.T) {
	f := newProcessingFixture(t)
	h := f.h.Handler()
	body := webhookBody{
		EventID:    "ev_" + newTestID(t),
		PayoutID:   f.payoutID,
		Reference:  f.transferID,
		Status:     eventStatusFailed,
		OccurredAt: f.fake.Now().UTC().Format(time.RFC3339),
	}
	assertHTTP200(t, postWebhook(t, h, f.providerID, body, testWebhookSecret, ""))
	tr, err := f.m.Get(context.Background(), f.transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tr.Status != transfer.StatusFailed {
		t.Fatalf("status %s want FAILED", tr.Status)
	}
	assertBalance(t, f.led, f.clientID, 1_000_000, 0)
	if n := countTransitionsTo(t, f.m, f.transferID, transfer.StatusFailed); n != 1 {
		t.Fatalf("FAILED transitions %d want 1", n)
	}
}
