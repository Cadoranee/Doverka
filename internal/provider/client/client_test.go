package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustRecipient(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func TestCreatePayout_amountCurrencyStringsAndRequestShape(t *testing.T) {
	bodies := make(chan []byte, 2)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			if r.Method != http.MethodPost {
				t.Errorf("method=%s", r.Method)
			}
			if r.URL.Path != "/prov_alpha/payouts" {
				t.Errorf("path=%s", r.URL.Path)
			}
			if r.Header.Get("Idempotency-Key") != "tr-9:2" {
				t.Errorf("Idempotency-Key=%q", r.Header.Get("Idempotency-Key"))
			}
			if r.Header.Get("X-Scenario") != "" {
				t.Errorf("X-Scenario=%q want empty", r.Header.Get("X-Scenario"))
			}
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		bodies <- b
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"payout_id":"po_1","status":"PROCESSING"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "http://api:8080/", nil)
	ctx := context.Background()
	req := CreateRequest{
		ProviderID: "prov_alpha",
		TransferID: "tr-9",
		Attempt:    2,
		Amount:     "124068.63",
		Currency:   "THB",
		Recipient:  mustRecipient(t, `{"name":"A","account":"40817810"}`),
	}
	res, err := c.CreatePayout(ctx, req)
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if res.Outcome != OutcomeCreated {
		t.Fatalf("Outcome=%q", res.Outcome)
	}
	if res.PayoutID != "po_1" || res.Status != "PROCESSING" {
		t.Fatalf("PayoutID=%q Status=%q", res.PayoutID, res.Status)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(<-bodies, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if string(raw["amount"]) != `"124068.63"` {
		t.Fatalf("amount raw=%s", raw["amount"])
	}
	if string(raw["currency"]) != `"THB"` {
		t.Fatalf("currency raw=%s", raw["currency"])
	}
	if string(raw["recipient"]) != `{"name":"A","account":"40817810"}` {
		t.Fatalf("recipient raw=%s", raw["recipient"])
	}
	if string(raw["reference"]) != `"tr-9"` {
		t.Fatalf("reference raw=%s", raw["reference"])
	}
	if string(raw["callback_url"]) != `"http://api:8080/webhooks/prov_alpha"` {
		t.Fatalf("callback_url raw=%s", raw["callback_url"])
	}

	req2 := CreateRequest{
		ProviderID: "prov_alpha",
		TransferID: "tr-10",
		Attempt:    1,
		Amount:     "100.10",
		Currency:   "JPY",
		Recipient:  mustRecipient(t, `{"name":"B"}`),
	}
	_, err = c.CreatePayout(ctx, req2)
	if err != nil {
		t.Fatalf("CreatePayout jpy: %v", err)
	}
	if err := json.Unmarshal(<-bodies, &raw); err != nil {
		t.Fatalf("unmarshal body2: %v", err)
	}
	if string(raw["amount"]) != `"100.10"` {
		t.Fatalf("amount2 raw=%s", raw["amount"])
	}
	if string(raw["currency"]) != `"JPY"` {
		t.Fatalf("currency2 raw=%s", raw["currency"])
	}
}

func TestCreatePayout_xScenarioHeaders(t *testing.T) {
	tests := []struct {
		scenario string
		want     string
	}{
		{"slow", "slow"},
		{"decline", "decline"},
	}
	for _, tc := range tests {
		t.Run(tc.scenario, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("X-Scenario"); got != tc.want {
					t.Errorf("X-Scenario=%q want %q", got, tc.want)
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"payout_id":"po_x","status":"PROCESSING"}`))
			}))
			defer srv.Close()
			c := New(srv.URL, "http://api:8080", nil)
			_, err := c.CreatePayout(context.Background(), CreateRequest{
				ProviderID: "p1",
				TransferID: "tr-1",
				Attempt:    1,
				Amount:     "1",
				Currency:   "USD",
				Recipient:  mustRecipient(t, `{"a":"b"}`),
				Scenario:   tc.scenario,
			})
			if err != nil {
				t.Fatalf("CreatePayout: %v", err)
			}
		})
	}
}

func TestCreatePayout_422Declined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"code":"RECIPIENT_ACCOUNT_INVALID","message":"recipient account is invalid"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	res, err := c.CreatePayout(context.Background(), validCreateReq())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.Outcome != OutcomeDeclined {
		t.Fatalf("Outcome=%q", res.Outcome)
	}
	if res.Code != "RECIPIENT_ACCOUNT_INVALID" {
		t.Fatalf("Code=%q", res.Code)
	}
}

func TestCreatePayout_5xxProviderError(t *testing.T) {
	for _, status := range []int{500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":"INTERNAL_ERROR","message":"x"}`))
			}))
			defer srv.Close()
			c := New(srv.URL, "http://api:8080", nil)
			res, err := c.CreatePayout(context.Background(), validCreateReq())
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if res.Outcome != OutcomeProviderError {
				t.Fatalf("Outcome=%q", res.Outcome)
			}
			if res.Code != "INTERNAL_ERROR" {
				t.Fatalf("Code=%q", res.Code)
			}
		})
	}
}

func TestCreatePayout_504Unknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	res, err := c.CreatePayout(context.Background(), validCreateReq())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.Outcome != OutcomeUnknown {
		t.Fatalf("Outcome=%q", res.Outcome)
	}
}

func TestCreatePayout_contextCancelUnknownFast(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-release:
		case <-timer.C:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"payout_id":"late","status":"PROCESSING"}`))
		}
	}))
	defer srv.Close()
	defer close(release)
	c := New(srv.URL, "http://api:8080", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := c.CreatePayout(ctx, validCreateReq())
	elapsed := time.Since(start)
	if elapsed >= 2*time.Second {
		t.Fatalf("elapsed=%v", elapsed)
	}
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.Outcome != OutcomeUnknown {
		t.Fatalf("Outcome=%q", res.Outcome)
	}
}

func TestCreatePayout_slowScenarioFast201Created(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scenario") != "slow" {
			t.Errorf("X-Scenario=%q", r.Header.Get("X-Scenario"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"payout_id":"po_fast","status":"PROCESSING"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	req := validCreateReq()
	req.Scenario = "slow"
	res, err := c.CreatePayout(context.Background(), req)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.Outcome != OutcomeCreated || res.PayoutID != "po_fast" {
		t.Fatalf("Outcome=%q PayoutID=%q", res.Outcome, res.PayoutID)
	}
}

func TestCreatePayout_409ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"IDEMPOTENCY_REUSED","message":"conflict"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	res, err := c.CreatePayout(context.Background(), validCreateReq())
	if err == nil {
		t.Fatalf("want error, res=%+v", res)
	}
	if res.Outcome == OutcomeDeclined {
		t.Fatalf("Outcome must not be declined")
	}
}

func TestCreatePayout_validationNoHTTP(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	ctx := context.Background()
	cases := []CreateRequest{
		{ProviderID: "p", TransferID: "t", Attempt: 0, Amount: "1", Currency: "USD", Recipient: mustRecipient(t, `{}`)},
		{ProviderID: "p", TransferID: "t", Attempt: 1, Amount: "", Currency: "USD", Recipient: mustRecipient(t, `{}`)},
		{ProviderID: "p", TransferID: "t", Attempt: 1, Amount: "1", Currency: "USD", Recipient: json.RawMessage(`[]`)},
		{ProviderID: "p", TransferID: "t", Attempt: 1, Amount: "1", Currency: "USD", Recipient: json.RawMessage(`not-json`)},
	}
	for i, req := range cases {
		_, err := c.CreatePayout(ctx, req)
		if err == nil {
			t.Fatalf("case %d: want error", i)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("server hits=%d", hits.Load())
	}
}

func TestGetPayout_successNotFoundTransport(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/p1/payouts/po_ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"payout_id":"po_ok","reference":"tr-1","status":"COMPLETED","failure_code":null}`))
	})
	mux.HandleFunc("/p1/payouts/missing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	ctx := context.Background()

	p, found, err := c.GetPayout(ctx, "p1", "po_ok")
	if err != nil || !found {
		t.Fatalf("GetPayout ok: found=%v err=%v", found, err)
	}
	if p.PayoutID != "po_ok" || p.Reference != "tr-1" || p.Status != "COMPLETED" {
		t.Fatalf("p=%+v", p)
	}

	_, found, err = c.GetPayout(ctx, "p1", "missing")
	if err != nil || found {
		t.Fatalf("GetPayout 404: found=%v err=%v", found, err)
	}

	srv.Close()
	_, found, err = c.GetPayout(ctx, "p1", "po_ok")
	if err == nil || found {
		t.Fatalf("GetPayout transport: found=%v err=%v", found, err)
	}
}

func TestFindPayout_queryAndResponses(t *testing.T) {
	refs := make(chan string, 2)
	mux := http.NewServeMux()
	mux.HandleFunc("/p2/payouts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		refs <- r.URL.Query().Get("reference")
		if r.URL.Query().Get("reference") == "tr-find" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_f","reference":"tr-find","status":"PROCESSING","failure_code":""}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	ctx := context.Background()

	p, found, err := c.FindPayout(ctx, "p2", "tr-find")
	if err != nil || !found {
		t.Fatalf("FindPayout ok: found=%v err=%v", found, err)
	}
	if gotRef := <-refs; gotRef != "tr-find" {
		t.Fatalf("reference query=%q", gotRef)
	}
	if p.PayoutID != "po_f" || p.Reference != "tr-find" || p.Status != "PROCESSING" {
		t.Fatalf("p=%+v", p)
	}

	_, found, err = c.FindPayout(ctx, "p2", "tr-missing")
	if err != nil || found {
		t.Fatalf("FindPayout 404: found=%v err=%v", found, err)
	}
	if gotRef := <-refs; gotRef != "tr-missing" {
		t.Fatalf("reference query=%q", gotRef)
	}
}

func TestCreatePayout_noReplayOnBrokenKeepAliveConnection(t *testing.T) {
	var postCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if strings.Count(r.URL.Path, "/") == 3 && strings.Contains(r.URL.Path, "/payouts/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		case http.MethodPost:
			if strings.HasSuffix(r.URL.Path, "/payouts") {
				n := postCount.Add(1)
				if _, err := io.ReadAll(r.Body); err != nil {
					t.Errorf("read body: %v", err)
				}
				if n == 1 {
					hj, ok := w.(http.Hijacker)
					if !ok {
						t.Fatal("server is not Hijacker")
					}
					conn, _, err := hj.Hijack()
					if err != nil {
						t.Fatalf("hijack: %v", err)
					}
					conn.Close()
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":"INTERNAL_ERROR","message":"x"}`))
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New(srv.URL, "http://api:8080", nil)
	ctx := context.Background()
	_, _, err := c.GetPayout(ctx, "prov_alpha", "warm-id")
	if err != nil {
		t.Fatalf("GetPayout: %v", err)
	}

	res, err := c.CreatePayout(ctx, validCreateReq())
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if res.Outcome != OutcomeUnknown {
		t.Fatalf("Outcome=%q want %q", res.Outcome, OutcomeUnknown)
	}
	if postCount.Load() != 1 {
		t.Fatalf("POST count=%d want 1", postCount.Load())
	}
}

func TestCreatePayout_transportErrorUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := New(url, "http://api:8080", nil)
	res, err := c.CreatePayout(context.Background(), validCreateReq())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res.Outcome != OutcomeUnknown || res.Detail == "" {
		t.Fatalf("res=%+v", res)
	}
}

func TestNew_nilHTTPClientFast201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"payout_id":"po_nil","status":"PROCESSING"}`))
	}))
	defer srv.Close()
	c := New(strings.TrimRight(srv.URL, "/"), "http://api:8080", nil)
	res, err := c.CreatePayout(context.Background(), validCreateReq())
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if res.Outcome != OutcomeCreated || res.PayoutID != "po_nil" {
		t.Fatalf("res=%+v", res)
	}
}

func validCreateReq() CreateRequest {
	return CreateRequest{
		ProviderID: "prov_alpha",
		TransferID: "tr-1",
		Attempt:    1,
		Amount:     "10",
		Currency:   "USD",
		Recipient:  json.RawMessage(`{"n":"v"}`),
	}
}
