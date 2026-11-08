package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
)

func fakePoll(accepted time.Time, window, interval time.Duration) (*clock.Fake, UnknownPoll) {
	fc := clock.NewFake(accepted)
	return fc, UnknownPoll{
		Window:   window,
		Interval: interval,
		Clock:    fc,
		Wait: func(ctx context.Context, d time.Duration) error {
			fc.Advance(d)
			return nil
		},
	}
}

func TestResolveUnknown_visibleAfterLagIsPresent(t *testing.T) {
	accepted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ref := "tr-lag"
	var posts atomic.Int32
	fc, poll := fakePoll(accepted, 15*time.Second, time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("/prov/payouts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			t.Error("unexpected POST")
		}
		if r.URL.Query().Get("reference") != ref {
			http.NotFound(w, r)
			return
		}
		if fc.Now().Before(accepted.Add(5 * time.Second)) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"payout_id":"po_lag","reference":"` + ref + `","status":"PROCESSING","failure_code":null}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)

	res, p, err := c.ResolveUnknown(context.Background(), "prov", ref, accepted, poll)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res != ResolutionPresent {
		t.Fatalf("Resolution=%q want present", res)
	}
	if p.PayoutID != "po_lag" || p.Status != "PROCESSING" {
		t.Fatalf("p=%+v", p)
	}
	if posts.Load() != 0 {
		t.Fatalf("posts=%d", posts.Load())
	}
}

func TestResolveUnknown_notFoundAfterWindowIsAbsent(t *testing.T) {
	accepted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ref := "tr-absent"
	_, poll := fakePoll(accepted, 15*time.Second, time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)

	res, _, err := c.ResolveUnknown(context.Background(), "prov", ref, accepted, poll)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res != ResolutionAbsent {
		t.Fatalf("Resolution=%q want absent", res)
	}
}

func TestResolveUnknown_notFoundWhenWindowAlreadyElapsedIsAbsent(t *testing.T) {
	accepted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ref := "tr-elapsed"
	fc := clock.NewFake(accepted.Add(15 * time.Second))
	poll := UnknownPoll{
		Window:   15 * time.Second,
		Interval: time.Second,
		Clock:    fc,
		Wait: func(ctx context.Context, d time.Duration) error {
			t.Error("unexpected wait after window elapsed")
			fc.Advance(d)
			return nil
		},
	}
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)

	res, _, err := c.ResolveUnknown(context.Background(), "prov", ref, accepted, poll)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res != ResolutionAbsent {
		t.Fatalf("Resolution=%q want absent", res)
	}
	if gets.Load() < 1 {
		t.Fatalf("gets=%d", gets.Load())
	}
}

func TestResolveUnknown_stillPresentAfterWindow(t *testing.T) {
	accepted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ref := "tr-late"
	fc := clock.NewFake(accepted.Add(20 * time.Second))
	poll := UnknownPoll{
		Window:   15 * time.Second,
		Interval: time.Second,
		Clock:    fc,
		Wait: func(ctx context.Context, d time.Duration) error {
			fc.Advance(d)
			return nil
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"payout_id":"po_late","reference":"` + ref + `","status":"PROCESSING","failure_code":null}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)

	res, p, err := c.ResolveUnknown(context.Background(), "prov", ref, accepted, poll)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res != ResolutionPresent {
		t.Fatalf("Resolution=%q want present", res)
	}
	if p.PayoutID != "po_late" {
		t.Fatalf("PayoutID=%q", p.PayoutID)
	}
}

func TestResolveUnknown_cancelBeforeWindowIsUnknown(t *testing.T) {
	accepted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ref := "tr-cancel"
	fc := clock.NewFake(accepted)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poll := UnknownPoll{
		Window:   15 * time.Second,
		Interval: time.Second,
		Clock:    fc,
		Wait: func(ctx context.Context, d time.Duration) error {
			cancel()
			return context.Canceled
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)

	res, _, err := c.ResolveUnknown(ctx, "prov", ref, accepted, poll)
	if res != ResolutionUnknown {
		t.Fatalf("Resolution=%q want unknown", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveUnknown_cancelDuringLookupIsUnknown(t *testing.T) {
	accepted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var once sync.Once
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	poll := UnknownPoll{
		Window:   15 * time.Second,
		Interval: time.Second,
		Clock:    clock.NewFake(accepted),
		Wait: func(context.Context, time.Duration) error {
			t.Fatal("canceled lookup must not wait")
			return nil
		},
	}
	c := New(srv.URL, "http://api:8080", nil)
	res, _, err := c.ResolveUnknown(ctx, "prov", "tr-abort", accepted, poll)
	if res != ResolutionUnknown {
		t.Fatalf("Resolution=%q want unknown", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveUnknown_serverErrorAfterWindowIsUnknown(t *testing.T) {
	accepted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ref := "tr-5xx"
	_, poll := fakePoll(accepted, 15*time.Second, time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)

	res, _, err := c.ResolveUnknown(context.Background(), "prov", ref, accepted, poll)
	if err == nil {
		t.Fatal("want error")
	}
	if res != ResolutionUnknown {
		t.Fatalf("Resolution=%q want unknown", res)
	}
}

func TestResolveUnknown_realClockNotFoundIsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	accepted := time.Now().UTC()
	poll := UnknownPoll{Window: 80 * time.Millisecond, Interval: 10 * time.Millisecond}

	res, _, err := c.ResolveUnknown(ctx, "prov", "tr-real", accepted, poll)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if res != ResolutionAbsent {
		t.Fatalf("Resolution=%q want absent", res)
	}
}

func TestResolveUnknown_defaultWindowIsFifteenSeconds(t *testing.T) {
	poll := DefaultUnknownPoll()
	if poll.Window != 15*time.Second {
		t.Fatalf("Window=%v", poll.Window)
	}
	if poll.Interval != time.Second {
		t.Fatalf("Interval=%v", poll.Interval)
	}
}

func TestResolveUnknown_invalidPollNoHTTP(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Error("server must not be called")
	}))
	defer srv.Close()
	c := New(srv.URL, "http://api:8080", nil)
	accepted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	poll := UnknownPoll{Window: 0, Interval: time.Second}

	res, _, err := c.ResolveUnknown(context.Background(), "prov", "ref", accepted, poll)
	if err == nil {
		t.Fatal("want error")
	}
	if res == ResolutionAbsent {
		t.Fatalf("Resolution=%q must not be absent", res)
	}
	if hits.Load() != 0 {
		t.Fatalf("hits=%d", hits.Load())
	}
}
