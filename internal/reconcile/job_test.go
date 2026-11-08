package reconcile

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/ledger"
	providerclient "github.com/Cadoranee/Doverka/internal/provider/client"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/jackc/pgx/v5"
)

func newReconcileJob(t *testing.T, db *store.DB, m *transfer.Machine, led *ledger.Ledger, fake *clock.Fake, payouts PayoutReader) *Job {
	t.Helper()
	return NewJob(db.Pool(), m, led, payouts, fake)
}

func newHTTPPayoutReader(t *testing.T, h http.Handler) PayoutReader {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return providerclient.New(srv.URL, "http://api.test", nil)
}

func requireOwnStaleProcessing(t *testing.T, db *store.DB, cutoff time.Time, allowed ...string) {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(), `
SELECT transfer_id FROM transfers
WHERE status = 'PROCESSING' AND updated_at < $1`, cutoff)
	if err != nil {
		t.Fatalf("list stale processing: %v", err)
	}
	defer rows.Close()
	own := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		own[id] = struct{}{}
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan stale processing: %v", err)
		}
		if _, ok := own[id]; !ok {
			t.Fatalf("stale PROCESSING transfer %s belongs to another run; clean up foreign rows or set DATABASE_URL to an isolated database", id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stale processing: %v", err)
	}
}

func staleCutoff(fake *clock.Fake, staleAfter time.Duration) time.Time {
	return fake.Now().UTC().Add(-staleAfter)
}

func advanceStale(t *testing.T, fake *clock.Fake, staleAfter time.Duration) {
	t.Helper()
	fake.Advance(staleAfter + time.Millisecond)
}

func lastAttemptResult(t *testing.T, db *store.DB, transferID string) string {
	t.Helper()
	var result string
	err := db.Pool().QueryRow(context.Background(), `
SELECT COALESCE(result, '') FROM provider_attempts
WHERE transfer_id = $1
ORDER BY id DESC
LIMIT 1`, transferID).Scan(&result)
	if err != nil {
		t.Fatalf("last attempt result: %v", err)
	}
	return result
}

type stubPayoutReader struct {
	get func(ctx context.Context, providerID, payoutID string) (providerclient.Payout, bool, error)
}

func (s stubPayoutReader) GetPayout(ctx context.Context, providerID, payoutID string) (providerclient.Payout, bool, error) {
	if s.get == nil {
		return providerclient.Payout{}, false, errors.New("stub not set")
	}
	return s.get(ctx, providerID, payoutID)
}

func TestJob_unconfigured(t *testing.T) {
	ctx := context.Background()
	j := NewJob(nil, nil, nil, nil, nil)
	if err := j.RunOnce(ctx); err == nil || err.Error() != "reconcile job is not configured" {
		t.Fatalf("RunOnce: %v", err)
	}
	if err := j.Run(ctx); err == nil || err.Error() != "reconcile job is not configured" {
		t.Fatalf("Run: %v", err)
	}
	db, m, led, fake := openResumeTest(t)
	j2 := NewJob(db.Pool(), m, led, stubPayoutReader{}, fake)
	j2.interval = 0
	if err := j2.RunOnce(ctx); err == nil || err.Error() != "reconcile job is not configured" {
		t.Fatalf("RunOnce zero interval: %v", err)
	}
}

func TestJob_runCancelledContext(t *testing.T) {
	var getCount atomic.Int32
	db, m, led, fake := openResumeTest(t)
	seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getCount.Add(1)
		}
		http.NotFound(w, r)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := j.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if getCount.Load() != 0 {
		t.Fatalf("GET count %d want 0", getCount.Load())
	}
}

func TestJob_freshProcessingNotPolled(t *testing.T) {
	var httpCalls atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := m.Get(context.Background(), nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != transfer.StatusProcessing {
		t.Fatalf("status %s want PROCESSING", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
	if httpCalls.Load() != 0 {
		t.Fatalf("HTTP calls %d want 0", httpCalls.Load())
	}
}

func TestJob_exactlyStaleAfterNotPolled(t *testing.T) {
	var httpCalls atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	fake.Advance(StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusProcessing {
		t.Fatalf("status %s want PROCESSING", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
	if httpCalls.Load() != 0 {
		t.Fatalf("HTTP calls %d want 0", httpCalls.Load())
	}
}

func TestJob_staleCompletedSettles(t *testing.T) {
	var postCount atomic.Int32
	var getCount atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
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
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
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
	if getCount.Load() != 1 {
		t.Fatalf("GET count %d want 1", getCount.Load())
	}
	if result := lastAttemptResult(t, db, nt.ID); result != "COMPLETED" {
		t.Fatalf("attempt result %q want COMPLETED", result)
	}
	firstGets := getCount.Load()
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce again: %v", err)
	}
	assertResumeBalance(t, led, clientID, 350000, 0)
	if getCount.Load() != firstGets {
		t.Fatalf("GET count increased on second RunOnce")
	}
}

func TestJob_staleFailedReleases(t *testing.T) {
	var postCount atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
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
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusFailed {
		t.Fatalf("status %s want FAILED", got.Status)
	}
	assertResumeBalance(t, led, clientID, 500000, 0)
	if postCount.Load() != 0 {
		t.Fatalf("POST count %d want 0", postCount.Load())
	}
	if lastAttemptResult(t, db, nt.ID) != "FAILED" {
		t.Fatalf("attempt result %q want FAILED", lastAttemptResult(t, db, nt.ID))
	}
}

func TestJob_staleStillProcessingUnchanged(t *testing.T) {
	var getCount atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_resume") {
			getCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_resume","reference":"` + nt.ID + `","status":"PROCESSING"}`))
			return
		}
		http.NotFound(w, r)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusProcessing {
		t.Fatalf("status %s want PROCESSING", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
	if getCount.Load() != 1 {
		t.Fatalf("GET count %d want 1", getCount.Load())
	}
}

func TestJob_getPayoutErrorLeavesProcessing(t *testing.T) {
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
	wantErr := errors.New("provider transport down")
	j := newReconcileJob(t, db, m, led, fake, stubPayoutReader{
		get: func(context.Context, string, string) (providerclient.Payout, bool, error) {
			return providerclient.Payout{}, false, wantErr
		},
	})
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	err := j.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusProcessing {
		t.Fatalf("status %s want PROCESSING", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
}

func TestJob_getPayoutNotFoundNeedsReview(t *testing.T) {
	var postCount atomic.Int32
	var getCount atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCount.Add(1)
		}
		if r.Method == http.MethodGet {
			getCount.Add(1)
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusNeedsReview {
		t.Fatalf("status %s want NEEDS_REVIEW", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
	if postCount.Load() != 0 {
		t.Fatalf("POST count %d want 0", postCount.Load())
	}
	if getCount.Load() != 1 {
		t.Fatalf("GET count %d want 1", getCount.Load())
	}
}

func TestJob_noPayoutIDNeedsReviewNoHTTP(t *testing.T) {
	var httpCalls atomic.Int32
	db, m, led, fake := openResumeTest(t)
	ctx := context.Background()
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	inTx(t, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
UPDATE provider_attempts SET payout_id = NULL, result = 'unknown'
WHERE transfer_id = $1`, nt.ID)
		return err
	})
	advanceStale(t, fake, StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusNeedsReview {
		t.Fatalf("status %s want NEEDS_REVIEW", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
	if httpCalls.Load() != 0 {
		t.Fatalf("HTTP calls %d want 0", httpCalls.Load())
	}
}

func TestJob_twoStaleOneTickFreshUntouched(t *testing.T) {
	ctx := context.Background()
	db, m, led, fake := openResumeTest(t)
	clientOK, ntOK := seedProcessingFixture(t, db, m, led, fake)
	clientFail, ntFail := seedProcessingFixture(t, db, m, led, fake)
	inTx(t, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
UPDATE provider_attempts SET payout_id = 'po_fail'
WHERE transfer_id = $1 AND payout_id = 'po_resume'`, ntFail.ID)
		return err
	})
	advanceStale(t, fake, StaleAfter)
	clientFresh, ntFresh := seedProcessingFixture(t, db, m, led, fake)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/payouts/po_resume"):
			_, _ = w.Write([]byte(`{"payout_id":"po_resume","reference":"` + ntOK.ID + `","status":"COMPLETED"}`))
		case strings.HasSuffix(r.URL.Path, "/payouts/po_fail"):
			_, _ = w.Write([]byte(`{"payout_id":"po_fail","reference":"` + ntFail.ID + `","status":"FAILED","failure_code":"PROVIDER_DECLINED"}`))
		default:
			http.NotFound(w, r)
		}
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), ntOK.ID, ntFail.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	gotOK, _ := m.Get(context.Background(), ntOK.ID)
	gotFail, _ := m.Get(context.Background(), ntFail.ID)
	gotFresh, _ := m.Get(context.Background(), ntFresh.ID)
	if gotOK.Status != transfer.StatusCompleted {
		t.Fatalf("completed transfer status %s", gotOK.Status)
	}
	if gotFail.Status != transfer.StatusFailed {
		t.Fatalf("failed transfer status %s", gotFail.Status)
	}
	if gotFresh.Status != transfer.StatusProcessing {
		t.Fatalf("fresh transfer status %s want PROCESSING", gotFresh.Status)
	}
	assertResumeBalance(t, led, clientOK, 350000, 0)
	assertResumeBalance(t, led, clientFail, 500000, 0)
	assertResumeBalance(t, led, clientFresh, 350000, 150000)
}

func TestJob_queuedStatusNeedsReview(t *testing.T) {
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_resume") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_resume","reference":"` + nt.ID + `","status":"QUEUED"}`))
			return
		}
		http.NotFound(w, r)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusNeedsReview {
		t.Fatalf("status %s want NEEDS_REVIEW", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
}

func TestJob_referenceMismatchHoldsReserve(t *testing.T) {
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_resume") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_resume","reference":"other-transfer","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusNeedsReview {
		t.Fatalf("status %s want NEEDS_REVIEW", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
}

func TestJob_backgroundRunPollsUntilCancel(t *testing.T) {
	var getCount atomic.Int32
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_resume") {
			getCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_resume","reference":"` + nt.ID + `","status":"PROCESSING"}`))
			return
		}
		http.NotFound(w, r)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	j.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- j.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for getCount.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("GET count %d want >= 2", getCount.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusProcessing {
		t.Fatalf("status %s want PROCESSING", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 150000)
}

func TestJob_concurrentRunOnceCompletedOnce(t *testing.T) {
	db, m, led, fake := openResumeTest(t)
	clientID, nt := seedProcessingFixture(t, db, m, led, fake)
	advanceStale(t, fake, StaleAfter)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payouts/po_resume") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"payout_id":"po_resume","reference":"` + nt.ID + `","status":"COMPLETED"}`))
			return
		}
		http.NotFound(w, r)
	})
	j := newReconcileJob(t, db, m, led, fake, newHTTPPayoutReader(t, h))
	requireOwnStaleProcessing(t, db, staleCutoff(fake, StaleAfter), nt.ID)
	var wg sync.WaitGroup
	var errs [2]error
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = j.RunOnce(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("RunOnce goroutine %d: %v", i, err)
		}
	}
	got, _ := m.Get(context.Background(), nt.ID)
	if got.Status != transfer.StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	assertResumeBalance(t, led, clientID, 350000, 0)
}
