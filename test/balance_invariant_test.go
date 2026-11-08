package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/ledger"
	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/shopspring/decimal"
)

const balanceInvariantClientID = "c_103"

type balancePair struct {
	available decimal.Decimal
	reserved  decimal.Decimal
}

type transferWithSourceAmount struct {
	TransferID   string        `json:"transfer_id"`
	Status       string        `json:"status"`
	SourceAmount scenarioMoney `json:"source_amount"`
}

func parseBalanceSnapshot(t *testing.T, body []byte) map[string]balancePair {
	t.Helper()
	var resp scenarioBalanceResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("balance json: %v", err)
	}
	out := make(map[string]balancePair, len(resp.Balances))
	for _, item := range resp.Balances {
		available, err := decimal.NewFromString(item.Available)
		if err != nil {
			t.Fatalf("balance %s available %q: %v", item.Currency, item.Available, err)
		}
		reserved, err := decimal.NewFromString(item.Reserved)
		if err != nil {
			t.Fatalf("balance %s reserved %q: %v", item.Currency, item.Reserved, err)
		}
		out[item.Currency] = balancePair{available: available, reserved: reserved}
	}
	return out
}

func fetchClientBalances(t *testing.T, baseURL string) map[string]balancePair {
	t.Helper()
	body, code := scenarioGET(t, baseURL, "/clients/"+balanceInvariantClientID+"/balance")
	if code != http.StatusOK {
		t.Fatalf("GET balance: status %d body %s", code, string(body))
	}
	return parseBalanceSnapshot(t, body)
}

func assertBalanceInvariant(
	t *testing.T,
	start, current map[string]balancePair,
	completedSum map[string]decimal.Decimal,
) {
	t.Helper()
	seen := make(map[string]struct{})
	for c := range start {
		seen[c] = struct{}{}
	}
	for c := range current {
		seen[c] = struct{}{}
	}
	for c := range completedSum {
		seen[c] = struct{}{}
	}
	for currency := range seen {
		s := start[currency]
		c := current[currency]
		sum := completedSum[currency]
		left := s.available.Add(s.reserved)
		right := c.available.Add(c.reserved).Add(sum)
		if !left.Equal(right) {
			t.Fatalf(
				"balance invariant %s: start_avail=%s start_reserved=%s current_avail=%s current_reserved=%s completed_sum=%s",
				currency, s.available, s.reserved, c.available, c.reserved, sum,
			)
		}
	}
}

func postSourceQuoteTransfer(t *testing.T, baseURL, amount, label, scenario string) string {
	t.Helper()
	quoteBody := map[string]string{
		"source_country":  "RU",
		"source_currency": "RUB",
		"target_country":  "TH",
		"target_currency": "THB",
		"payout_method":   "BANK_ACCOUNT",
		"amount":          amount,
		"amount_type":     "SOURCE",
	}
	qRaw, qCode := scenarioPOST(t, baseURL, "/quotes", nil, quoteBody)
	if qCode != http.StatusCreated {
		t.Fatalf("POST /quotes: status %d body %s", qCode, string(qRaw))
	}
	var qResp scenarioQuoteResponse
	if err := json.Unmarshal(qRaw, &qResp); err != nil {
		t.Fatalf("quote json: %v", err)
	}
	if qResp.QuoteID == "" {
		t.Fatal("empty quote_id")
	}
	transferBody := map[string]any{
		"quote_id":  qResp.QuoteID,
		"client_id": balanceInvariantClientID,
		"recipient": map[string]string{
			"account": "40817810",
			"name":    "Invariant",
		},
	}
	headers := map[string]string{
		"Idempotency-Key": fmt.Sprintf("bal-inv-%s-%d", label, time.Now().UnixNano()),
		"X-Scenario":      scenario,
	}
	trRaw, trCode := scenarioPOST(t, baseURL, "/transfers", headers, transferBody)
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
	return trResp.TransferID
}

func getTransferWithSource(t *testing.T, baseURL, transferID string) transferWithSourceAmount {
	t.Helper()
	body, code := scenarioGET(t, baseURL, "/transfers/"+transferID)
	if code != http.StatusOK {
		t.Fatalf("GET /transfers/%s: status %d body %s", transferID, code, string(body))
	}
	var tr transferWithSourceAmount
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatalf("transfer json: %v", err)
	}
	return tr
}

func sourceAmount(t *testing.T, tr transferWithSourceAmount) (currency string, amount decimal.Decimal) {
	t.Helper()
	currency = tr.SourceAmount.Currency
	if currency == "" {
		t.Fatalf("transfer %s: source_amount.currency is empty", tr.TransferID)
	}
	parsed, err := decimal.NewFromString(tr.SourceAmount.Amount)
	if err != nil {
		t.Fatalf("transfer %s: source_amount.amount %q: %v", tr.TransferID, tr.SourceAmount.Amount, err)
	}
	if !parsed.IsPositive() {
		t.Fatalf("transfer %s: source_amount %s %s must be positive", tr.TransferID, parsed, currency)
	}
	return currency, parsed
}

func TestBalanceInvariant_Session(t *testing.T) {
	dbURL, mockURL, root := requireScenarioEnv(t)
	db := openScenarioDB(t, dbURL)
	dataDir := filepath.Join(root, "data")
	cat, err := refdata.Load(dataDir)
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	resetClientForScenario(t, db, ledger.New(db.Pool()), cat, balanceInvariantClientID)
	srv := buildScenarioServer(t, db, mockURL, dataDir)

	start := fetchClientBalances(t, srv.URL)
	if _, ok := start["RUB"]; !ok {
		t.Fatal("start balance: RUB missing")
	}
	if _, ok := start["KWD"]; !ok {
		t.Fatal("start balance: KWD missing")
	}

	completedSum := map[string]decimal.Decimal{}

	okID := postSourceQuoteTransfer(t, srv.URL, "1000.00", "ok", "ok")
	pollScenarioTransfer(t, srv.URL, okID, "COMPLETED", 45*time.Second)
	okCur, okAmt := sourceAmount(t, getTransferWithSource(t, srv.URL, okID))
	if okCur != "RUB" {
		t.Fatalf("COMPLETED source currency: got %s want RUB", okCur)
	}
	completedSum[okCur] = completedSum[okCur].Add(okAmt)
	assertBalanceInvariant(t, start, fetchClientBalances(t, srv.URL), completedSum)

	declineID := postSourceQuoteTransfer(t, srv.URL, "2500.00", "decline", "decline")
	pollScenarioTransfer(t, srv.URL, declineID, "FAILED", 45*time.Second)
	declineCur, declineAmt := sourceAmount(t, getTransferWithSource(t, srv.URL, declineID))
	if declineCur != "RUB" || declineAmt.Equal(okAmt) {
		t.Fatalf("FAILED source_amount: got %s %s completed=%s %s", declineAmt, declineCur, okAmt, okCur)
	}
	afterDecline := fetchClientBalances(t, srv.URL)
	assertBalanceInvariant(t, start, afterDecline, completedSum)

	slowID := postSourceQuoteTransfer(t, srv.URL, "1750.00", "slow", "slow")
	slowTr := getTransferWithSource(t, srv.URL, slowID)
	switch slowTr.Status {
	case "COMPLETED", "FAILED", "NEEDS_REVIEW":
		t.Fatalf("slow transfer: status %q before poll; in-flight reserve not observed", slowTr.Status)
	}
	mid := fetchClientBalances(t, srv.URL)
	assertBalanceInvariant(t, start, mid, completedSum)
	if !mid["RUB"].reserved.GreaterThan(afterDecline["RUB"].reserved) {
		t.Fatalf("slow in-flight RUB reserved: after decline %s current %s", afterDecline["RUB"].reserved, mid["RUB"].reserved)
	}

	pollScenarioTransfer(t, srv.URL, slowID, "COMPLETED", 90*time.Second)
	slowCur, slowAmt := sourceAmount(t, getTransferWithSource(t, srv.URL, slowID))
	if slowCur != "RUB" {
		t.Fatalf("slow COMPLETED source currency: got %s want RUB", slowCur)
	}
	completedSum[slowCur] = completedSum[slowCur].Add(slowAmt)
	assertBalanceInvariant(t, start, fetchClientBalances(t, srv.URL), completedSum)
}
