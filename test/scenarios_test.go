package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/httpapi"
	"github.com/Cadoranee/Doverka/internal/idempotency"
	"github.com/Cadoranee/Doverka/internal/ledger"
	providerclient "github.com/Cadoranee/Doverka/internal/provider/client"
	selection "github.com/Cadoranee/Doverka/internal/provider/select"
	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/rates"
	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/shopspring/decimal"
)

type scenarioMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type scenarioQuoteResponse struct {
	QuoteID      string        `json:"quote_id"`
	SourceAmount scenarioMoney `json:"source_amount"`
}

type scenarioBalanceItem struct {
	Currency  string `json:"currency"`
	Available string `json:"available"`
	Reserved  string `json:"reserved"`
}

type scenarioBalanceResponse struct {
	ClientID string                `json:"client_id"`
	Balances []scenarioBalanceItem `json:"balances"`
}

type scenarioCreateTransferResponse struct {
	TransferID string `json:"transfer_id"`
	Status     string `json:"status"`
}

type scenarioProviderAttempt struct {
	ProviderID  string `json:"provider_id"`
	AttemptedAt string `json:"attempted_at"`
	Result      string `json:"result"`
	PayoutID    string `json:"payout_id"`
}

type scenarioTransferGet struct {
	TransferID       string                    `json:"transfer_id"`
	Status           string                    `json:"status"`
	ProviderAttempts []scenarioProviderAttempt `json:"provider_attempts"`
}

func requireScenarioEnv(t *testing.T) (dbURL, mockURL, root string) {
	t.Helper()
	dbURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	mockURL = strings.TrimSpace(os.Getenv("MOCK_PROVIDER_URL"))
	if dbURL == "" || mockURL == "" {
		t.Skip("DATABASE_URL or MOCK_PROVIDER_URL is not set")
	}
	root = findRepoRoot(t)
	healthURL := strings.TrimRight(mockURL, "/") + "/health"
	resp, err := http.Get(healthURL)
	if err != nil {
		t.Fatalf("mock health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mock health: status %d", resp.StatusCode)
	}
	return dbURL, mockURL, root
}

func openScenarioDB(t *testing.T, dbURL string) *store.DB {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func buildScenarioServer(t *testing.T, db *store.DB, mockURL, dataDir string) *httptest.Server {
	t.Helper()
	cat, err := refdata.Load(dataDir)
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	providers, err := selection.LoadProviders(filepath.Join(dataDir, "providers.json"))
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	clk := clock.System{}
	quoteRepo := quote.NewRepo(db.Pool(), clk)
	machine := transfer.New(db.Pool(), clk)
	led := ledger.New(db.Pool())
	rateSvc := rates.New(rates.NewHTTPSource(mockURL, nil), clk)

	h := &httpapi.Handler{
		Catalog:      cat,
		Balances:     led,
		Rates:        rateSvc,
		Store:        quoteRepo,
		Clock:        clk,
		DB:           db.Pool(),
		Transfers:    machine,
		TransferView: machine,
		Ledger:       led,
		Idempotency:  idempotency.New(db.Pool()),
	}
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)

	pc := providerclient.New(mockURL, srv.URL, nil)
	exec := transfer.NewExecutor(db.Pool(), machine, led, pc, clk)
	h.Runner = transfer.NewRunner(exec, machine, quoteRepo, cat, providers, rateSvc, clk)
	return srv
}

func resetClientForScenario(t *testing.T, db *store.DB, led *ledger.Ledger, cat *refdata.Catalog, clientID string) {
	t.Helper()
	ctx := context.Background()
	pool := db.Pool()
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("delete ledger_entries: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM provider_attempts WHERE transfer_id IN (SELECT transfer_id FROM transfers WHERE client_id = $1)`, clientID); err != nil {
		t.Fatalf("delete provider_attempts: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM transfer_transitions WHERE transfer_id IN (SELECT transfer_id FROM transfers WHERE client_id = $1)`, clientID); err != nil {
		t.Fatalf("delete transfer_transitions: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM transfers WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("delete transfers: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM client_balances WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("delete client_balances: %v", err)
	}
	cl, ok := cat.Client(clientID)
	if !ok {
		t.Fatalf("client %q not in catalog", clientID)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	for currency, amount := range cl.Balances {
		cur, ok := cat.Currency(currency)
		if !ok {
			t.Fatalf("unknown currency %q", currency)
		}
		minor := amount.Shift(int32(cur.MinorUnits)).IntPart()
		if err := led.Seed(ctx, tx, clientID, currency, minor); err != nil {
			t.Fatalf("seed %s: %v", currency, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

func scenarioRUBBalance(t *testing.T, body []byte) (available, reserved decimal.Decimal) {
	t.Helper()
	var resp scenarioBalanceResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("balance json: %v", err)
	}
	for _, item := range resp.Balances {
		if item.Currency != "RUB" {
			continue
		}
		return decimal.RequireFromString(item.Available), decimal.RequireFromString(item.Reserved)
	}
	t.Fatal("RUB balance not found")
	return decimal.Zero, decimal.Zero
}

func scenarioGET(t *testing.T, baseURL, path string) ([]byte, int) {
	t.Helper()
	resp, err := http.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body, resp.StatusCode
}

func scenarioPOST(t *testing.T, baseURL, path string, headers map[string]string, payload any) ([]byte, int) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body, resp.StatusCode
}

func pollScenarioTransfer(t *testing.T, baseURL, transferID, want string, deadline time.Duration) scenarioTransferGet {
	t.Helper()
	dead := time.Now().Add(deadline)
	var last scenarioTransferGet
	var lastBody []byte
	for time.Now().Before(dead) {
		body, code := scenarioGET(t, baseURL, "/transfers/"+transferID)
		if code != http.StatusOK {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		lastBody = body
		if err := json.Unmarshal(body, &last); err != nil {
			t.Fatalf("transfer json: %v", err)
		}
		if last.Status == want {
			return last
		}
		switch last.Status {
		case "COMPLETED", "FAILED", "NEEDS_REVIEW":
			t.Fatalf("poll transfer: status %s, want %s, body %s", last.Status, want, string(body))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("poll transfer: want status %s, last status %q, body %s", want, last.Status, string(lastBody))
	return last
}

type scenarioOpts struct {
	clientID      string
	scenario      string
	wantTerminal  string
	pollDeadline  time.Duration
	maxPOST       time.Duration
	checkAttempts bool
}

func runScenarioFlow(t *testing.T, srv *httptest.Server, cat *refdata.Catalog, opts scenarioOpts) {
	t.Helper()
	quoteBody := map[string]string{
		"source_country":  "RU",
		"source_currency": "RUB",
		"target_country":  "TH",
		"target_currency": "THB",
		"payout_method":   "BANK_ACCOUNT",
		"amount":          "1000.00",
		"amount_type":     "SOURCE",
	}
	qRaw, qCode := scenarioPOST(t, srv.URL, "/quotes", nil, quoteBody)
	if qCode != http.StatusCreated {
		t.Fatalf("POST /quotes: status %d body %s", qCode, string(qRaw))
	}
	var qResp scenarioQuoteResponse
	if err := json.Unmarshal(qRaw, &qResp); err != nil {
		t.Fatalf("quote json: %v", err)
	}
	sourceDebit := decimal.RequireFromString(qResp.SourceAmount.Amount)

	balRaw, balCode := scenarioGET(t, srv.URL, "/clients/"+opts.clientID+"/balance")
	if balCode != http.StatusOK {
		t.Fatalf("GET balance: status %d", balCode)
	}
	preAvail, preRes := scenarioRUBBalance(t, balRaw)
	if !preRes.IsZero() {
		t.Fatalf("pre-transfer reserved: got %s want 0", preRes)
	}
	cl, ok := cat.Client(opts.clientID)
	if !ok {
		t.Fatalf("client %q", opts.clientID)
	}
	wantSeed, ok := cl.Balances["RUB"]
	if !ok {
		t.Fatal("client RUB seed missing")
	}
	if !preAvail.Equal(wantSeed) {
		t.Fatalf("pre-transfer available: got %s want %s", preAvail, wantSeed)
	}

	idemKey := fmt.Sprintf("scenario-%s-%d", opts.scenario, time.Now().UnixNano())
	transferBody := map[string]any{
		"quote_id":  qResp.QuoteID,
		"client_id": opts.clientID,
		"recipient": map[string]string{
			"account": "40817810",
			"name":    "Scenario",
		},
	}
	headers := map[string]string{
		"Idempotency-Key": idemKey,
		"X-Scenario":      opts.scenario,
	}
	start := time.Now()
	trRaw, trCode := scenarioPOST(t, srv.URL, "/transfers", headers, transferBody)
	elapsed := time.Since(start)
	if opts.maxPOST > 0 && elapsed > opts.maxPOST {
		t.Fatalf("POST /transfers took %s, want under %s", elapsed, opts.maxPOST)
	}
	if trCode != http.StatusAccepted {
		t.Fatalf("POST /transfers: status %d body %s", trCode, string(trRaw))
	}
	var trResp scenarioCreateTransferResponse
	if err := json.Unmarshal(trRaw, &trResp); err != nil {
		t.Fatalf("transfer create json: %v", err)
	}
	if trResp.TransferID == "" {
		t.Fatal("empty transfer_id")
	}
	switch trResp.Status {
	case "COMPLETED", "FAILED", "NEEDS_REVIEW":
		t.Fatalf("202 status %q must not be terminal", trResp.Status)
	}

	detail := pollScenarioTransfer(t, srv.URL, trResp.TransferID, opts.wantTerminal, opts.pollDeadline)

	balAfterRaw, balAfterCode := scenarioGET(t, srv.URL, "/clients/"+opts.clientID+"/balance")
	if balAfterCode != http.StatusOK {
		t.Fatalf("GET balance after: status %d", balAfterCode)
	}
	afterAvail, afterRes := scenarioRUBBalance(t, balAfterRaw)

	switch opts.wantTerminal {
	case "COMPLETED":
		if !afterRes.IsZero() {
			t.Fatalf("post-transfer reserved: got %s want 0", afterRes)
		}
		wantAvail := preAvail.Sub(sourceDebit)
		if !afterAvail.Equal(wantAvail) {
			t.Fatalf("post-transfer available: got %s want %s", afterAvail, wantAvail)
		}
	case "FAILED":
		if !afterRes.IsZero() {
			t.Fatalf("post-transfer reserved: got %s want 0", afterRes)
		}
		if !afterAvail.Equal(preAvail) {
			t.Fatalf("post-transfer available: got %s want %s", afterAvail, preAvail)
		}
	default:
		t.Fatalf("unknown wantTerminal %q", opts.wantTerminal)
	}

	if opts.checkAttempts {
		if len(detail.ProviderAttempts) == 0 {
			t.Fatal("provider_attempts empty")
		}
		for _, a := range detail.ProviderAttempts {
			if strings.TrimSpace(a.PayoutID) != "" {
				t.Fatalf("payout_id must be empty, got %q", a.PayoutID)
			}
		}
	}
}

func TestScenario_okCompletedDebitsAvailable(t *testing.T) {
	dbURL, mockURL, root := requireScenarioEnv(t)
	db := openScenarioDB(t, dbURL)
	dataDir := filepath.Join(root, "data")
	cat, err := refdata.Load(dataDir)
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	resetClientForScenario(t, db, ledger.New(db.Pool()), cat, "c_001")
	srv := buildScenarioServer(t, db, mockURL, dataDir)
	runScenarioFlow(t, srv, cat, scenarioOpts{
		clientID:     "c_001",
		scenario:     "ok",
		wantTerminal: "COMPLETED",
		pollDeadline: 45 * time.Second,
	})
}

func TestScenario_declineFailsAndRestoresAvailable(t *testing.T) {
	dbURL, mockURL, root := requireScenarioEnv(t)
	db := openScenarioDB(t, dbURL)
	dataDir := filepath.Join(root, "data")
	cat, err := refdata.Load(dataDir)
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	resetClientForScenario(t, db, ledger.New(db.Pool()), cat, "c_002")
	srv := buildScenarioServer(t, db, mockURL, dataDir)
	runScenarioFlow(t, srv, cat, scenarioOpts{
		clientID:     "c_002",
		scenario:     "decline",
		wantTerminal: "FAILED",
		pollDeadline: 30 * time.Second,
	})
}

func TestScenario_error5xxFailsWithoutPayoutAndRestoresAvailable(t *testing.T) {
	dbURL, mockURL, root := requireScenarioEnv(t)
	db := openScenarioDB(t, dbURL)
	dataDir := filepath.Join(root, "data")
	cat, err := refdata.Load(dataDir)
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	resetClientForScenario(t, db, ledger.New(db.Pool()), cat, "c_003")
	srv := buildScenarioServer(t, db, mockURL, dataDir)
	runScenarioFlow(t, srv, cat, scenarioOpts{
		clientID:      "c_003",
		scenario:      "error_5xx",
		wantTerminal:  "FAILED",
		pollDeadline:  30 * time.Second,
		checkAttempts: true,
	})
}

func TestScenario_slowReturns202BeforeCompletion(t *testing.T) {
	dbURL, mockURL, root := requireScenarioEnv(t)
	db := openScenarioDB(t, dbURL)
	dataDir := filepath.Join(root, "data")
	cat, err := refdata.Load(dataDir)
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	resetClientForScenario(t, db, ledger.New(db.Pool()), cat, "c_006")
	srv := buildScenarioServer(t, db, mockURL, dataDir)
	runScenarioFlow(t, srv, cat, scenarioOpts{
		clientID:     "c_006",
		scenario:     "slow",
		wantTerminal: "COMPLETED",
		pollDeadline: 90 * time.Second,
		maxPOST:      10 * time.Second,
	})
}
