package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/ledger"
	providerclient "github.com/Cadoranee/Doverka/internal/provider/client"
	selection "github.com/Cadoranee/Doverka/internal/provider/select"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

func cleanupExecute(t *testing.T, db *store.DB, clientID, transferID, quoteID string) {
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
	if _, err := db.Pool().Exec(ctx, `DELETE FROM daily_usage WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("cleanup daily_usage: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM client_balances WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("cleanup client_balances: %v", err)
	}
}

func reservedTransferFixture(t *testing.T, db *store.DB, m *Machine, led *ledger.Ledger) (clientID string, nt NewTransfer) {
	t.Helper()
	ctx := context.Background()
	clientID = "c_" + newTestID(t)
	quoteID := newTestID(t)
	insertQuote(t, db.Pool(), quoteID)
	nt = NewTransfer{
		ID:                newTestID(t),
		QuoteID:           quoteID,
		ClientID:          clientID,
		SourceAmountMinor: 150000,
		SourceCurrency:    "RUB",
		TargetAmountMinor: 54945,
		TargetCurrency:    "THB",
		Recipient:         json.RawMessage(`{"account":"TH1234"}`),
	}
	inTx(t, db, func(tx pgx.Tx) error {
		if err := led.Seed(ctx, tx, clientID, "RUB", 500000); err != nil {
			return err
		}
		if _, _, err := lockClientBalance(ctx, tx, clientID, "RUB"); err != nil {
			return err
		}
		if _, err := m.Create(ctx, tx, nt); err != nil {
			return err
		}
		if err := led.Reserve(ctx, tx, clientID, "RUB", nt.ID, nt.SourceAmountMinor); err != nil {
			return err
		}
		_, err := m.Transition(ctx, tx, nt.ID, StatusReserved, "reserved")
		return err
	})
	return clientID, nt
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

func newExecuteEnv(t *testing.T, handler http.Handler) (*Executor, *Machine, *ledger.Ledger, *store.DB) {
	t.Helper()
	db, m, fake := openTransferTest(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	pc := providerclient.New(srv.URL, "http://api.test", nil)
	led := ledger.New(db.Pool())
	exec := NewExecutor(db.Pool(), m, led, pc, fake)
	exec.unknownPoll = providerclient.UnknownPoll{
		Window:   300 * time.Millisecond,
		Interval: 20 * time.Millisecond,
		Clock:    fake,
		Wait: func(ctx context.Context, d time.Duration) error {
			fake.Advance(d)
			return nil
		},
	}
	exec.pollInterval = 10 * time.Millisecond
	return exec, m, led, db
}

func TestExecute_okScenarioCompletesAndDebits(t *testing.T) {
	const providerID = "prov_ok"
	var captured struct {
		amount   string
		currency string
		scenario string
		decode   error
	}
	var postRef string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/payouts") {
			captured.scenario = r.Header.Get("X-Scenario")
			var body struct {
				Amount    string `json:"amount"`
				Currency  string `json:"currency"`
				Reference string `json:"reference"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				captured.decode = err
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			captured.amount = body.Amount
			captured.currency = body.Currency
			postRef = body.Reference
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"payout_id":"po_ok","status":"PROCESSING"}`))
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_ok") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_ok","reference":"` + postRef + `","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})

	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	ranked := selection.Ranked{
		Primary: []selection.Candidate{{Provider: selection.Provider{ID: providerID}}},
	}
	err := exec.Execute(context.Background(), ExecuteInput{
		TransferID: nt.ID,
		Scenario:   "ok",
		MinorUnits: 2,
		Ranked:     ranked,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if captured.decode != nil {
		t.Fatalf("decode post: %v", captured.decode)
	}
	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertBalance(t, led, clientID, 350000, 0)
	if captured.amount != "549.45" || captured.currency != "THB" {
		t.Fatalf("post amount/currency: %q %q want 549.45 THB", captured.amount, captured.currency)
	}
	if captured.scenario != "ok" {
		t.Fatalf("X-Scenario %q want ok", captured.scenario)
	}
	attempts, err := exec.Attempts(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts len %d want 1", len(attempts))
	}
	if attempts[0].ProviderID != providerID || attempts[0].Result != "COMPLETED" || attempts[0].PayoutID != "po_ok" {
		t.Fatalf("attempt: %+v", attempts[0])
	}
}

type integrationMinorScale map[string]int

func (s integrationMinorScale) MinorUnits(currency string) (int, bool) {
	u, ok := s[currency]
	return u, ok
}

func TestExecute_failedReleasesDailyUsage(t *testing.T) {
	const providerID = "prov_decline_daily"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":"RECIPIENT_ACCOUNT_INVALID","message":"bad"}`))
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	scale := integrationMinorScale{"RUB": 2, "THB": 2}
	ctx := context.Background()
	inTx(t, db, func(tx pgx.Tx) error {
		return led.ReserveDaily(ctx, tx, ledger.DailyCharge{
			ClientID:          clientID,
			At:                got.CreatedAt,
			SourceCurrency:    nt.SourceCurrency,
			SourceAmountMinor: nt.SourceAmountMinor,
			LimitAmount:       decimal.RequireFromString("1000000.00"),
			LimitCurrency:     "RUB",
		}, scale, nil)
	})

	err = exec.Execute(context.Background(), ExecuteInput{
		TransferID: nt.ID,
		Scenario:   "decline",
		MinorUnits: 2,
		Ranked:     selection.Ranked{Primary: []selection.Candidate{{Provider: selection.Provider{ID: providerID}}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ = m.Get(context.Background(), nt.ID)
	if got.Status != StatusFailed {
		t.Fatalf("status %s want FAILED", got.Status)
	}
	buckets, err := led.DailyUsage(ctx, clientID, got.CreatedAt)
	if err != nil {
		t.Fatalf("DailyUsage: %v", err)
	}
	if len(buckets) != 0 {
		t.Fatalf("daily usage not released: %+v", buckets)
	}
}

func TestExecute_declineRestoresAvailable(t *testing.T) {
	const providerID = "prov_decline"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":"RECIPIENT_ACCOUNT_INVALID","message":"recipient account is invalid"}`))
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	err := exec.Execute(context.Background(), ExecuteInput{
		TransferID: nt.ID,
		Scenario:   "decline",
		MinorUnits: 2,
		Ranked:     selection.Ranked{Primary: []selection.Candidate{{Provider: selection.Provider{ID: providerID}}}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != StatusFailed {
		t.Fatalf("status %s want FAILED", got.Status)
	}
	assertBalance(t, led, clientID, 500000, 0)
	attempts, _ := exec.Attempts(context.Background(), nt.ID)
	if len(attempts) != 1 || attempts[0].Result != "RECIPIENT_ACCOUNT_INVALID" || attempts[0].PayoutID != "" {
		t.Fatalf("attempts: %+v", attempts)
	}
}

func TestExecute_error5xxExhaustsProvidersAndRestores(t *testing.T) {
	const p1, p2 = "prov_one", "prov_two"
	var order []string
	var mu sync.Mutex
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			mu.Lock()
			order = append(order, parts[0])
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"INTERNAL_ERROR","message":"internal provider error"}`))
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	err := exec.Execute(context.Background(), ExecuteInput{
		TransferID: nt.ID,
		Scenario:   "error_5xx",
		MinorUnits: 2,
		Ranked: selection.Ranked{
			Primary: []selection.Candidate{
				{Provider: selection.Provider{ID: p1}},
				{Provider: selection.Provider{ID: p2}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != StatusFailed {
		t.Fatalf("status %s want FAILED", got.Status)
	}
	assertBalance(t, led, clientID, 500000, 0)
	attempts, _ := exec.Attempts(context.Background(), nt.ID)
	if len(attempts) != 2 {
		t.Fatalf("attempts len %d want 2", len(attempts))
	}
	if attempts[0].ProviderID != p1 || attempts[0].Result != "INTERNAL_ERROR" || attempts[0].PayoutID != "" {
		t.Fatalf("attempt0: %+v", attempts[0])
	}
	if attempts[1].ProviderID != p2 || attempts[1].Result != "INTERNAL_ERROR" || attempts[1].PayoutID != "" {
		t.Fatalf("attempt1: %+v", attempts[1])
	}
	if len(order) != 2 || order[0] != p1 || order[1] != p2 {
		t.Fatalf("provider order %v want [%s %s]", order, p1, p2)
	}
}

func TestExecute_recipientAccountInvalidTriesNextProvider(t *testing.T) {
	const p1, p2 = "prov_bad", "prov_good"
	var postRef string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		prov := parts[0]
		if r.Method == http.MethodPost {
			if prov == p1 {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"code":"RECIPIENT_ACCOUNT_INVALID","message":"recipient account is invalid"}`))
				return
			}
			var body struct {
				Reference string `json:"reference"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postRef = body.Reference
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"payout_id":"po_good","status":"PROCESSING"}`))
			return
		}
		if r.Method == http.MethodGet && prov == p2 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_good","reference":"` + postRef + `","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	err := exec.Execute(context.Background(), ExecuteInput{
		TransferID: nt.ID,
		MinorUnits: 2,
		Ranked: selection.Ranked{
			Primary: []selection.Candidate{
				{Provider: selection.Provider{ID: p1}},
				{Provider: selection.Provider{ID: p2}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertBalance(t, led, clientID, 350000, 0)
	attempts, _ := exec.Attempts(context.Background(), nt.ID)
	if len(attempts) != 2 {
		t.Fatalf("attempts len %d want 2", len(attempts))
	}
	if attempts[0].Result != "RECIPIENT_ACCOUNT_INVALID" || attempts[1].Result != "COMPLETED" || attempts[1].PayoutID != "po_good" {
		t.Fatalf("attempts: %+v", attempts)
	}
}

func TestExecute_slowScenarioDoesNotBlockCaller(t *testing.T) {
	const providerID = "prov_slow"
	unblock := make(chan struct{})
	var postSeen atomic.Bool
	var postRef string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body struct {
				Reference string `json:"reference"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postRef = body.Reference
			postSeen.Store(true)
			<-unblock
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"payout_id":"po_slow","status":"PROCESSING"}`))
			return
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_slow","reference":"` + postRef + `","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	ctx := context.Background()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, ExecuteInput{
			TransferID: nt.ID,
			Scenario:   "slow",
			MinorUnits: 2,
			Ranked:     selection.Ranked{Primary: []selection.Candidate{{Provider: selection.Provider{ID: providerID}}}},
		})
	}()

	for !postSeen.Load() {
		if time.Since(start) > 5*time.Second {
			t.Fatal("timed out waiting for provider POST")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed >= 30*time.Second {
		t.Fatalf("observation took %v, must be < 30s", elapsed)
	}
	got, err := m.Get(ctx, nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Terminal() {
		t.Fatalf("status %s should not be terminal while POST blocked", got.Status)
	}
	if got.Status != StatusSubmitting {
		t.Fatalf("status %s want SUBMITTING", got.Status)
	}
	assertBalance(t, led, clientID, 350000, 150000)

	close(unblock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Execute did not finish")
	}
	got, _ = m.Get(ctx, nt.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertBalance(t, led, clientID, 350000, 0)
}

func TestExecute_unknownOutcomeReferenceFindsPayoutCompletes(t *testing.T) {
	const providerID = "prov_unknown_hit"
	const p2 = "prov_unused"
	var postRef string
	var postProviders []string
	var mu sync.Mutex
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 0 {
			http.NotFound(w, r)
			return
		}
		prov := parts[0]
		if r.Method == http.MethodPost {
			mu.Lock()
			postProviders = append(postProviders, prov)
			mu.Unlock()
			var body struct {
				Reference string `json:"reference"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postRef = body.Reference
			if prov == providerID {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && prov == providerID && r.URL.Query().Get("reference") != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_found","reference":"` + postRef + `","status":"COMPLETED"}`))
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "po_found") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_found","reference":"` + postRef + `","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	err := exec.Execute(context.Background(), ExecuteInput{
		TransferID: nt.ID,
		MinorUnits: 2,
		Ranked: selection.Ranked{
			Primary: []selection.Candidate{
				{Provider: selection.Provider{ID: providerID}},
				{Provider: selection.Provider{ID: p2}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertBalance(t, led, clientID, 350000, 0)
	attempts, _ := exec.Attempts(context.Background(), nt.ID)
	if len(attempts) != 1 || attempts[0].PayoutID != "po_found" || attempts[0].Result != "COMPLETED" {
		t.Fatalf("attempts: %+v", attempts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(postProviders) != 1 || postProviders[0] != providerID {
		t.Fatalf("post providers %v want only %s", postProviders, providerID)
	}
}

func TestExecute_unknownAbsentFailsOverToNextProvider(t *testing.T) {
	const p1, p2 = "prov_gone", "prov_ok"
	var postRef string
	var postOrder []string
	var mu sync.Mutex
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		prov := parts[0]
		if r.Method == http.MethodPost {
			mu.Lock()
			postOrder = append(postOrder, prov)
			mu.Unlock()
			var body struct {
				Reference string `json:"reference"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postRef = body.Reference
			if prov == p1 {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"payout_id":"po_ok","status":"PROCESSING"}`))
			return
		}
		if r.Method == http.MethodGet && prov == p1 && r.URL.Query().Get("reference") != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && prov == p2 && strings.HasSuffix(r.URL.Path, "po_ok") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_ok","reference":"` + postRef + `","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	err := exec.Execute(context.Background(), ExecuteInput{
		TransferID: nt.ID,
		MinorUnits: 2,
		Ranked: selection.Ranked{
			Primary: []selection.Candidate{
				{Provider: selection.Provider{ID: p1}},
				{Provider: selection.Provider{ID: p2}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	attempts, _ := exec.Attempts(context.Background(), nt.ID)
	if len(attempts) != 2 || attempts[0].Result != selection.PayoutAbsent || attempts[1].Result != "COMPLETED" {
		t.Fatalf("attempts: %+v", attempts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(postOrder) != 2 || postOrder[0] != p1 || postOrder[1] != p2 {
		t.Fatalf("post order %v", postOrder)
	}
}

func TestExecute_unknownReferencePoll500StaysSubmitting(t *testing.T) {
	const providerID = "prov_poll_err"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		prov := parts[0]
		if r.Method == http.MethodPost && prov == providerID {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		if r.Method == http.MethodGet && prov == providerID && r.URL.Query().Get("reference") != "" {
			http.Error(w, "provider down", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	err := exec.Execute(context.Background(), ExecuteInput{
		TransferID: nt.ID,
		MinorUnits: 2,
		Ranked:     selection.Ranked{Primary: []selection.Candidate{{Provider: selection.Provider{ID: providerID}}}},
	})
	if err == nil || !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Execute err %v want ErrOutcomeUnknown", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != StatusSubmitting {
		t.Fatalf("status %s want SUBMITTING", got.Status)
	}
	attempts, _ := exec.Attempts(context.Background(), nt.ID)
	if len(attempts) != 1 || attempts[0].Result != attemptResultUnknown {
		t.Fatalf("attempts: %+v", attempts)
	}
	assertBalance(t, led, clientID, 350000, 150000)
}

func TestExecute_settleNoopsWhenTransferAlreadyTerminal(t *testing.T) {
	const providerID = "prov_webhook_race"
	processingStarted := make(chan struct{}, 1)
	var postRef string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body struct {
				Reference string `json:"reference"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			postRef = body.Reference
			select {
			case processingStarted <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"payout_id":"po_race","status":"PROCESSING"}`))
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "po_race") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_race","reference":"` + postRef + `","status":"PROCESSING"}`))
			return
		}
		http.NotFound(w, r)
	})
	exec, m, led, db := newExecuteEnv(t, h)
	clientID, nt := reservedTransferFixture(t, db, m, led)
	t.Cleanup(func() { cleanupExecute(t, db, clientID, nt.ID, nt.QuoteID) })

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, ExecuteInput{
			TransferID: nt.ID,
			MinorUnits: 2,
			Ranked:     selection.Ranked{Primary: []selection.Candidate{{Provider: selection.Provider{ID: providerID}}}},
		})
	}()
	select {
	case <-processingStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for payout create")
	}
	for {
		got, err := m.Get(ctx, nt.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status == StatusProcessing {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	inTx(t, db, func(tx pgx.Tx) error {
		if _, _, err := lockClientBalance(ctx, tx, clientID, "RUB"); err != nil {
			return err
		}
		if err := led.Debit(ctx, tx, clientID, "RUB", nt.ID, nt.SourceAmountMinor); err != nil {
			return err
		}
		_, err := m.Transition(ctx, tx, nt.ID, StatusCompleted, "webhook")
		return err
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Execute did not finish")
	}
	got, _ := m.Get(ctx, nt.ID)
	if got.Status != StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertBalance(t, led, clientID, 350000, 0)
}
