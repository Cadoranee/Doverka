package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cadoranee/Doverka/internal/ledger"
)

type fakeBalanceLister struct {
	balances []ledger.Balance
	err      error
}

func (f *fakeBalanceLister) List(_ context.Context, _ string) ([]ledger.Balance, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.balances, nil
}

type noCallBalanceLister struct {
	t *testing.T
}

func (n *noCallBalanceLister) List(context.Context, string) ([]ledger.Balance, error) {
	n.t.Fatal("List must not be called")
	return nil, nil
}

func balanceTestHandler(t *testing.T, lister BalanceLister) http.Handler {
	t.Helper()
	return (&Handler{
		Catalog:  loadCatalog(t),
		Balances: lister,
	}).Routes()
}

func getClientBalance(t *testing.T, h http.Handler, clientID string) (*httptest.ResponseRecorder, clientBalanceResponse, apiErrorBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/clients/"+clientID+"/balance", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		var errBody apiErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
			t.Fatalf("unmarshal error: %v body=%s", err, rec.Body.String())
		}
		return rec, clientBalanceResponse{}, errBody
	}
	var ok clientBalanceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	return rec, ok, apiErrorBody{}
}

func getClientBalanceError(t *testing.T, h http.Handler, clientID string) (*httptest.ResponseRecorder, apiErrorBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/clients/"+clientID+"/balance", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var errBody apiErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("unmarshal error: %v body=%s", err, rec.Body.String())
	}
	return rec, errBody
}

func TestGetClientBalance_returnsAvailableAndReservedPerCurrency(t *testing.T) {
	h := balanceTestHandler(t, &fakeBalanceLister{balances: []ledger.Balance{
		{ClientID: "c_001", Currency: "RUB", Available: 8500000, Reserved: 1500000},
		{ClientID: "c_001", Currency: "KWD", Available: 15000000, Reserved: 1000},
	}})
	rec, body, _ := getClientBalance(t, h, "c_001")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content-type=%q", rec.Header().Get("Content-Type"))
	}
	if body.ClientID != "c_001" {
		t.Fatalf("client_id=%q", body.ClientID)
	}
	if len(body.Balances) != 2 {
		t.Fatalf("len=%d", len(body.Balances))
	}
	if body.Balances[0].Currency != "KWD" || body.Balances[0].Available != "15000.000" || body.Balances[0].Reserved != "1.000" {
		t.Fatalf("KWD: %+v", body.Balances[0])
	}
	if body.Balances[1].Currency != "RUB" || body.Balances[1].Available != "85000.00" || body.Balances[1].Reserved != "15000.00" {
		t.Fatalf("RUB: %+v", body.Balances[1])
	}
	raw := rec.Body.String()
	for _, frag := range []string{
		`"available":"15000.000"`,
		`"reserved":"1.000"`,
		`"available":"85000.00"`,
		`"reserved":"15000.00"`,
	} {
		if !strings.Contains(raw, frag) {
			t.Fatalf("missing %q in %s", frag, raw)
		}
	}
	if strings.Contains(raw, `"available":85000`) || strings.Contains(raw, `"reserved":15000`) {
		t.Fatalf("amounts must be JSON strings, got %s", raw)
	}
}

func TestGetClientBalance_unknownClient(t *testing.T) {
	h := balanceTestHandler(t, &noCallBalanceLister{t: t})
	rec, errBody := getClientBalanceError(t, h, "missing")
	assertAPIError(t, rec, errBody, http.StatusNotFound, "CLIENT_NOT_FOUND")
}

func TestGetClientBalance_emptyLedgerIsEmptyArray(t *testing.T) {
	h := balanceTestHandler(t, &fakeBalanceLister{balances: nil})
	rec, body, _ := getClientBalance(t, h, "c_002")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body.ClientID != "c_002" {
		t.Fatalf("client_id=%q", body.ClientID)
	}
	if body.Balances == nil || len(body.Balances) != 0 {
		t.Fatalf("balances=%v", body.Balances)
	}
	if !strings.Contains(rec.Body.String(), `"balances":[]`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGetClientBalance_listerError(t *testing.T) {
	h := balanceTestHandler(t, &fakeBalanceLister{err: errors.New("list failed")})
	rec, errBody := getClientBalanceError(t, h, "c_001")
	assertAPIError(t, rec, errBody, http.StatusInternalServerError, "INTERNAL")
}

func TestGetClientBalance_unknownCurrency(t *testing.T) {
	h := balanceTestHandler(t, &fakeBalanceLister{balances: []ledger.Balance{
		{ClientID: "c_001", Currency: "ZZZ", Available: 1, Reserved: 0},
	}})
	rec, errBody := getClientBalanceError(t, h, "c_001")
	assertAPIError(t, rec, errBody, http.StatusInternalServerError, "INTERNAL")
}
