package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/ledger"
	providerclient "github.com/Cadoranee/Doverka/internal/provider/client"
	selection "github.com/Cadoranee/Doverka/internal/provider/select"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const resumeProviderID = "prov_resume"

type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

type executorContinuer struct {
	exec       *transfer.Executor
	minorUnits int
	ranked     selection.Ranked
}

func (c *executorContinuer) Run(ctx context.Context, transferID, scenario string) error {
	return c.exec.Execute(ctx, transfer.ExecuteInput{
		TransferID: transferID,
		Scenario:   scenario,
		MinorUnits: c.minorUnits,
		Ranked:     c.ranked,
	})
}

func openResumeTest(t *testing.T) (*store.DB, *transfer.Machine, *ledger.Ledger, *clock.Fake) {
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
	m := transfer.New(db.Pool(), fake)
	led := ledger.New(db.Pool())
	return db, m, led, fake
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

func cleanupResume(t *testing.T, db *store.DB, clientID, transferID, quoteID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool().Exec(ctx, `DELETE FROM provider_attempts WHERE transfer_id = $1`, transferID); err != nil {
		t.Fatalf("cleanup provider_attempts: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM transfer_transitions WHERE transfer_id = $1`, transferID); err != nil {
		t.Fatalf("cleanup transfer_transitions: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM ledger_entries WHERE client_id = $1 OR transfer_id = $2`, clientID, transferID); err != nil {
		t.Fatalf("cleanup ledger_entries: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM transfers WHERE transfer_id = $1`, transferID); err != nil {
		t.Fatalf("cleanup transfers: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM quotes WHERE quote_id = $1`, quoteID); err != nil {
		t.Fatalf("cleanup quotes: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM client_balances WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("cleanup client_balances: %v", err)
	}
}

func requireOwnInFlight(t *testing.T, db *store.DB, allowed ...string) {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(), `
SELECT transfer_id FROM transfers
WHERE status IN ('RESERVED', 'SUBMITTING', 'PROCESSING')`)
	if err != nil {
		t.Fatalf("list in-flight: %v", err)
	}
	defer rows.Close()
	own := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		own[id] = struct{}{}
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan in-flight: %v", err)
		}
		if _, ok := own[id]; !ok {
			t.Fatalf("in-flight transfer %s belongs to another run; refusing to resume it", id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate in-flight: %v", err)
	}
}

func assertResumeBalance(t *testing.T, led *ledger.Ledger, clientID string, available, reserved int64) {
	t.Helper()
	b, err := led.Balance(context.Background(), clientID, "RUB")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if b.Available != available || b.Reserved != reserved {
		t.Fatalf("balance available=%d reserved=%d want %d/%d", b.Available, b.Reserved, available, reserved)
	}
}

func newTransferNT(t *testing.T, clientID, quoteID, transferID string) transfer.NewTransfer {
	t.Helper()
	return transfer.NewTransfer{
		ID:                transferID,
		QuoteID:           quoteID,
		ClientID:          clientID,
		SourceAmountMinor: 150000,
		SourceCurrency:    "RUB",
		TargetAmountMinor: 54945,
		TargetCurrency:    "THB",
		Recipient:         json.RawMessage(`{"account":"TH1234"}`),
	}
}

func seedReservedFixture(t *testing.T, db *store.DB, m *transfer.Machine, led *ledger.Ledger) (clientID string, nt transfer.NewTransfer) {
	t.Helper()
	ctx := context.Background()
	clientID = "c_" + newTestID(t)
	quoteID := newTestID(t)
	insertQuote(t, db.Pool(), quoteID)
	nt = newTransferNT(t, clientID, quoteID, newTestID(t))
	t.Cleanup(func() { cleanupResume(t, db, clientID, nt.ID, quoteID) })
	inTx(t, db, func(tx pgx.Tx) error {
		if err := led.Seed(ctx, tx, clientID, "RUB", 500000); err != nil {
			return err
		}
		if _, err := led.LockBalance(ctx, tx, clientID, "RUB"); err != nil {
			return err
		}
		if _, err := m.Create(ctx, tx, nt); err != nil {
			return err
		}
		if err := led.Reserve(ctx, tx, clientID, "RUB", nt.ID, nt.SourceAmountMinor); err != nil {
			return err
		}
		_, err := m.Transition(ctx, tx, nt.ID, transfer.StatusReserved, "reserved")
		return err
	})
	return clientID, nt
}

func seedProcessingFixture(t *testing.T, db *store.DB, m *transfer.Machine, led *ledger.Ledger, fake *clock.Fake) (clientID string, nt transfer.NewTransfer) {
	t.Helper()
	ctx := context.Background()
	clientID, nt = seedReservedFixture(t, db, m, led)
	inTx(t, db, func(tx pgx.Tx) error {
		if _, err := led.LockBalance(ctx, tx, clientID, "RUB"); err != nil {
			return err
		}
		if _, err := m.Transition(ctx, tx, nt.ID, transfer.StatusSubmitting, resumeProviderID); err != nil {
			return err
		}
		idemKey := nt.ID + ":1"
		if _, err := tx.Exec(ctx, `
INSERT INTO provider_attempts (transfer_id, provider_id, idempotency_key, payout_id, result, attempted_at)
VALUES ($1, $2, $3, $4, 'PROCESSING', $5)`,
			nt.ID, resumeProviderID, idemKey, "po_resume", fake.Now().UTC(),
		); err != nil {
			return err
		}
		_, err := m.Transition(ctx, tx, nt.ID, transfer.StatusProcessing, "po_resume")
		return err
	})
	return clientID, nt
}

func newResumeWorker(t *testing.T, db *store.DB, m *transfer.Machine, led *ledger.Ledger, fake *clock.Fake, h http.Handler) *Worker {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	pc := providerclient.New(srv.URL, "http://api.test", nil)
	exec := transfer.NewExecutor(db.Pool(), m, led, pc, fake)
	cont := &executorContinuer{
		exec:       exec,
		minorUnits: 2,
		ranked: selection.Ranked{
			Primary: []selection.Candidate{{Provider: selection.Provider{ID: resumeProviderID}}},
		},
	}
	return New(db.Pool(), m, led, cont)
}

func TestResume_unconfigured(t *testing.T) {
	err := New(nil, nil, nil, nil).Resume(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "resume worker is not configured" {
		t.Fatalf("error %q", err.Error())
	}
}

func TestResume_processingCompletesWithoutSecondPayout(t *testing.T) {
	var postCount atomic.Int32
	var getCount atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCount.Add(1)
			http.Error(w, "unexpected post", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_resume") {
			getCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_resume","reference":"` + nt.ID + `","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})
	w := newResumeWorker(t, db, m, led, fake, h)
	requireOwnInFlight(t, db, nt.ID)

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 0)
	if postCount.Load() != 0 {
		t.Fatalf("POST count %d want 0", postCount.Load())
	}
	firstGets := getCount.Load()
	if firstGets == 0 {
		t.Fatal("expected at least one GET")
	}

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume again: %v", err)
	}
	got, _ = m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusCompleted {
		t.Fatalf("status after second resume %s want COMPLETED", got.Status)
	}
	if getCount.Load() != firstGets {
		t.Fatalf("GET count increased from %d to %d on second resume", firstGets, getCount.Load())
	}
}

func TestResume_reservedContinuesToCompleted(t *testing.T) {
	var postCount atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedReservedFixture(t, db, m, led)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/payouts") {
			postCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"payout_id":"po_new","status":"PROCESSING"}`))
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_new") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_new","reference":"` + nt.ID + `","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})
	w := newResumeWorker(t, db, m, led, fake, h)
	requireOwnInFlight(t, db, nt.ID)

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if postCount.Load() != 1 {
		t.Fatalf("POST count %d want 1", postCount.Load())
	}
	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 0)
}

func TestResume_submittingUnknownBecomesNeedsReview(t *testing.T) {
	db, m, led, fake := openResumeTest(t)
	ctx := context.Background()
	clientID, nt := seedReservedFixture(t, db, m, led)

	inTx(t, db, func(tx pgx.Tx) error {
		if _, err := led.LockBalance(ctx, tx, clientID, "RUB"); err != nil {
			return err
		}
		if _, err := m.Transition(ctx, tx, nt.ID, transfer.StatusSubmitting, resumeProviderID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
INSERT INTO provider_attempts (transfer_id, provider_id, idempotency_key, payout_id, result, attempted_at)
VALUES ($1, $2, $3, NULL, 'unknown', $4)`,
			nt.ID, resumeProviderID, nt.ID+":1", fake.Now().UTC(),
		)
		return err
	})

	var httpCalls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	w := newResumeWorker(t, db, m, led, fake, h)
	requireOwnInFlight(t, db, nt.ID)

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != transfer.StatusNeedsReview {
		t.Fatalf("status %s want NEEDS_REVIEW", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
	if httpCalls.Load() != 0 {
		t.Fatalf("HTTP calls %d want 0", httpCalls.Load())
	}
}

func TestResume_processingFailureReleasesReserve(t *testing.T) {
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)

	var postCount atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCount.Add(1)
			http.Error(w, "unexpected post", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_resume") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_resume","reference":"` + nt.ID + `","status":"FAILED","failure_code":"PROVIDER_DECLINED"}`))
			return
		}
		http.NotFound(w, r)
	})
	w := newResumeWorker(t, db, m, led, fake, h)
	requireOwnInFlight(t, db, nt.ID)

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != transfer.StatusFailed {
		t.Fatalf("status %s want FAILED", got.Status)
	}
	assertResumeBalance(t, led, clientID, 500000, 0)
	if postCount.Load() != 0 {
		t.Fatalf("POST count %d want 0", postCount.Load())
	}
}

func TestResume_completedIsUntouched(t *testing.T) {
	db, m, led, fake := openResumeTest(t)
	ctx := context.Background()
	clientID := "c_" + newTestID(t)
	quoteID := newTestID(t)
	insertQuote(t, db.Pool(), quoteID)
	transferID := newTestID(t)
	nt := newTransferNT(t, clientID, quoteID, transferID)
	t.Cleanup(func() { cleanupResume(t, db, clientID, transferID, quoteID) })

	inTx(t, db, func(tx pgx.Tx) error {
		if err := led.Seed(ctx, tx, clientID, "RUB", 500000); err != nil {
			return err
		}
		if _, err := led.LockBalance(ctx, tx, clientID, "RUB"); err != nil {
			return err
		}
		if _, err := m.Create(ctx, tx, nt); err != nil {
			return err
		}
		if err := led.Reserve(ctx, tx, clientID, "RUB", transferID, nt.SourceAmountMinor); err != nil {
			return err
		}
		if _, err := m.Transition(ctx, tx, transferID, transfer.StatusReserved, "reserved"); err != nil {
			return err
		}
		if _, err := m.Transition(ctx, tx, transferID, transfer.StatusSubmitting, "submit"); err != nil {
			return err
		}
		if _, err := m.Transition(ctx, tx, transferID, transfer.StatusProcessing, "process"); err != nil {
			return err
		}
		if err := led.Debit(ctx, tx, clientID, "RUB", transferID, nt.SourceAmountMinor); err != nil {
			return err
		}
		_, err := m.Transition(ctx, tx, transferID, transfer.StatusCompleted, "completed")
		return err
	})

	var httpCalls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	w := newResumeWorker(t, db, m, led, fake, h)
	requireOwnInFlight(t, db)

	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, err := m.Get(context.Background(), transferID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 0)
	if httpCalls.Load() != 0 {
		t.Fatalf("HTTP calls %d want 0", httpCalls.Load())
	}
}

type errContinuer struct {
	err error
}

func (c errContinuer) Run(context.Context, string, string) error {
	return c.err
}

func TestWorker_Run_unconfigured(t *testing.T) {
	err := (*Worker)(nil).Run(context.Background(), "id", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "resume worker is not configured" {
		t.Fatalf("error %q", err.Error())
	}
}

func TestWorker_Run_unknownOutcomeNeedsReview(t *testing.T) {
	db, m, led, _ := openResumeTest(t)
	ctx := context.Background()
	clientID, nt := seedReservedFixture(t, db, m, led)

	inTx(t, db, func(tx pgx.Tx) error {
		if _, err := led.LockBalance(ctx, tx, clientID, "RUB"); err != nil {
			return err
		}
		_, err := m.Transition(ctx, tx, nt.ID, transfer.StatusSubmitting, resumeProviderID)
		return err
	})

	w := New(db.Pool(), m, led, errContinuer{err: transfer.ErrOutcomeUnknown})
	if err := w.Run(context.Background(), nt.ID, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != transfer.StatusNeedsReview {
		t.Fatalf("status %s want NEEDS_REVIEW", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
}

func TestWorker_Run_nilContinuerError(t *testing.T) {
	db, m, led, _ := openResumeTest(t)
	_, nt := seedReservedFixture(t, db, m, led)

	w := New(db.Pool(), m, led, errContinuer{err: nil})
	if err := w.Run(context.Background(), nt.ID, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestResume_otherErrorStaysReserved(t *testing.T) {
	db, m, led, _ := openResumeTest(t)
	clientID, nt := seedReservedFixture(t, db, m, led)
	requireOwnInFlight(t, db, nt.ID)

	w := New(db.Pool(), m, led, errContinuer{err: errors.New("db down")})
	if err := w.Resume(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != transfer.StatusReserved {
		t.Fatalf("status %s want RESERVED", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
}
