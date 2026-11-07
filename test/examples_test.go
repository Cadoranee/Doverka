package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/httpapi"
	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/rates"
	"github.com/Cadoranee/Doverka/internal/refdata"
)

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		examplesDir := filepath.Join(dir, "examples")
		dataDir := filepath.Join(dir, "data")
		if statDir(examplesDir) && statDir(dataDir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found (need examples/ and data/)")
		}
		dir = parent
	}
}

func statDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

type memStore struct {
	mu   sync.Mutex
	seq  int
	clk  clock.Clock
	byID map[string]quote.Quote
}

func (s *memStore) Save(_ context.Context, snap quote.Snapshot, ttl time.Duration) (quote.Quote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := fmt.Sprintf("q-%d", s.seq)
	now := s.clk.Now()
	copySnap := quote.Snapshot{
		DirectionID:          snap.DirectionID,
		SourceCountry:        snap.SourceCountry,
		SourceCurrency:       snap.SourceCurrency,
		TargetCountry:        snap.TargetCountry,
		TargetCurrency:       snap.TargetCurrency,
		PayoutMethod:         snap.PayoutMethod,
		AmountType:           snap.AmountType,
		SourceAmountMinor:    snap.SourceAmountMinor,
		FeeAmountMinor:       snap.FeeAmountMinor,
		FeeCurrency:          snap.FeeCurrency,
		AmountToConvertMinor: snap.AmountToConvertMinor,
		Rate:                 snap.Rate,
		TargetAmountMinor:    snap.TargetAmountMinor,
		Breakdown:            append(json.RawMessage(nil), snap.Breakdown...),
	}
	if s.byID == nil {
		s.byID = make(map[string]quote.Quote)
	}
	q := quote.Quote{
		ID:        id,
		Snapshot:  copySnap,
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
	}
	s.byID[id] = q
	return q, nil
}

func (s *memStore) Get(_ context.Context, quoteID string) (quote.Quote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.byID[quoteID]
	if !ok {
		return quote.Quote{}, quote.ErrNotFound
	}
	return q, nil
}

func setupHandler(t *testing.T, root string) http.Handler {
	t.Helper()
	ratesPath := filepath.Join(root, "data", "rates.json")
	ratesData, err := os.ReadFile(ratesPath)
	if err != nil {
		t.Fatalf("read rates.json: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rates" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(ratesData)
	}))
	t.Cleanup(srv.Close)

	fixedClock := clock.NewFake(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	rateSvc := rates.New(
		rates.NewHTTPSource(srv.URL, srv.Client()),
		fixedClock,
	)
	cat, err := refdata.Load(filepath.Join(root, "data"))
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	return (&httpapi.Handler{
		Catalog: cat,
		Rates:   rateSvc,
		Store:   &memStore{clk: fixedClock},
	}).Routes()
}

type exampleFile struct {
	Request  json.RawMessage `json:"request"`
	Response struct {
		Status *int            `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"response"`
}

func decodeJSONValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func skipQuoteField(key string) bool {
	return key == "quote_id" || key == "expires_at"
}

func jsonSubsetMatch(path string, expected, actual any) error {
	switch exp := expected.(type) {
	case map[string]any:
		act, ok := actual.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object, got %T", path, actual)
		}
		for key, ev := range exp {
			if skipQuoteField(key) {
				continue
			}
			av, ok := act[key]
			if !ok {
				return fmt.Errorf("%s: missing key %q", path, key)
			}
			if err := jsonSubsetMatch(path+"."+key, ev, av); err != nil {
				return err
			}
		}
	case []any:
		act, ok := actual.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array, got %T", path, actual)
		}
		if len(exp) != len(act) {
			return fmt.Errorf("%s: array length %d != %d", path, len(exp), len(act))
		}
		for i := range exp {
			if err := jsonSubsetMatch(fmt.Sprintf("%s[%d]", path, i), exp[i], act[i]); err != nil {
				return err
			}
		}
	case json.Number:
		act, ok := actual.(json.Number)
		if !ok || exp.String() != act.String() {
			return fmt.Errorf("%s: mismatch %#v != %#v", path, exp, actual)
		}
	default:
		if exp != actual {
			return fmt.Errorf("%s: mismatch %#v != %#v", path, exp, actual)
		}
	}
	return nil
}

func failBodyDiff(t *testing.T, file string, expected, actual any, matchErr error) {
	t.Helper()
	expB, _ := json.MarshalIndent(expected, "", "  ")
	actB, _ := json.MarshalIndent(actual, "", "  ")
	t.Fatalf("%s: %v\n--- expected (subset) ---\n%s\n--- actual ---\n%s", file, matchErr, expB, actB)
}

func postQuote(t *testing.T, h http.Handler, reqBody []byte) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/quotes", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestExamples(t *testing.T) {
	root := findRepoRoot(t)
	h := setupHandler(t, root)
	pattern := filepath.Join(root, "examples", "*.json")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no examples in %s", pattern)
	}
	sort.Strings(paths)

	for _, path := range paths {
		file := filepath.Base(path)
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var ex exampleFile
			if err := json.Unmarshal(raw, &ex); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			status, body := postQuote(t, h, ex.Request)
			if ex.Response.Status != nil && status != *ex.Response.Status {
				t.Fatalf("%s: HTTP status %d, want %d\nbody: %s", file, status, *ex.Response.Status, strings.TrimSpace(string(body)))
			}
			expected, err := decodeJSONValue(ex.Response.Body)
			if err != nil {
				t.Fatalf("decode expected body: %v", err)
			}
			actual, err := decodeJSONValue(body)
			if err != nil {
				t.Fatalf("decode actual body: %v raw=%s", err, body)
			}
			if err := jsonSubsetMatch(file, expected, actual); err != nil {
				failBodyDiff(t, file, expected, actual, err)
			}
		})
	}
}
