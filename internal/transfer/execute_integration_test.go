package transfer

import (
	"context"
	"encoding/json"
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
