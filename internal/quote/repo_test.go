package quote

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/shopspring/decimal"
)

func openQuoteTestRepo(t *testing.T) (*store.DB, *Repo, time.Time) {
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
	fakeNow := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	return db, NewRepo(db.Pool(), clock.NewFake(fakeNow)), fakeNow
}

func cleanupQuote(t *testing.T, db *store.DB, quoteID string) {
	t.Helper()
	if quoteID == "" {
		return
	}
	ctx := context.Background()
	_, err := db.Pool().Exec(ctx, `DELETE FROM quotes WHERE quote_id = $1`, quoteID)
	if err != nil {
		t.Fatalf("cleanup quote %s: %v", quoteID, err)
	}
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		DirectionID:          "RU-TH-BANK",
		SourceCountry:        "RU",
		SourceCurrency:       "RUB",
		TargetCountry:        "TH",
		TargetCurrency:       "THB",
		PayoutMethod:         "BANK_ACCOUNT",
		AmountType:           "SOURCE",
		SourceAmountMinor:    150000,
		FeeAmountMinor:       1500,
		FeeCurrency:          "RUB",
		AmountToConvertMinor: 148500,
		Rate:                 decimal.RequireFromString("0.3700"),
		TargetAmountMinor:    54945,
		Breakdown:            json.RawMessage(`["fee","fx"]`),
	}
}

func TestSave_twoIdenticalSnapshotsGetDifferentIDs(t *testing.T) {
	db, repo, fakeNow := openQuoteTestRepo(t)
	ctx := context.Background()
	snap := sampleSnapshot()
	const ttl = 900 * time.Second

	q1, err := repo.Save(ctx, snap, ttl)
	if err != nil {
		t.Fatalf("Save first: %v", err)
	}
	t.Cleanup(func() { cleanupQuote(t, db, q1.ID) })

	q2, err := repo.Save(ctx, snap, ttl)
	if err != nil {
		t.Fatalf("Save second: %v", err)
	}
	t.Cleanup(func() { cleanupQuote(t, db, q2.ID) })

	if q1.ID == q2.ID {
		t.Fatalf("quote IDs: got same %q, want different", q1.ID)
	}
	wantExpires := fakeNow.Add(ttl)
	if !q1.ExpiresAt.Equal(wantExpires) || !q2.ExpiresAt.Equal(wantExpires) {
		t.Errorf("ExpiresAt: q1=%v q2=%v, want %v", q1.ExpiresAt, q2.ExpiresAt, wantExpires)
	}
	if !q1.CreatedAt.Equal(fakeNow) || !q2.CreatedAt.Equal(fakeNow) {
		t.Errorf("CreatedAt: q1=%v q2=%v, want %v", q1.CreatedAt, q2.CreatedAt, fakeNow)
	}
	if q1.Expired(fakeNow) {
		t.Error("Expired(fakeNow): want false")
	}
	if !q1.Expired(wantExpires) {
		t.Error("Expired(expiresAt): want true")
	}
	if q1.Expired(wantExpires.Add(-time.Microsecond)) {
		t.Error("Expired(expiresAt - 1µs): want false")
	}
	got1, err := repo.Get(ctx, q1.ID)
	if err != nil {
		t.Fatalf("Get q1: %v", err)
	}
	got2, err := repo.Get(ctx, q2.ID)
	if err != nil {
		t.Fatalf("Get q2: %v", err)
	}
	if !got1.ExpiresAt.Equal(wantExpires) || !got2.ExpiresAt.Equal(wantExpires) {
		t.Errorf("stored ExpiresAt: q1=%v q2=%v, want %v", got1.ExpiresAt, got2.ExpiresAt, wantExpires)
	}
	if !got1.CreatedAt.Equal(fakeNow) || !got2.CreatedAt.Equal(fakeNow) {
		t.Errorf("stored CreatedAt: q1=%v q2=%v, want %v", got1.CreatedAt, got2.CreatedAt, fakeNow)
	}
}

func TestGet_returnsSavedSnapshotFields(t *testing.T) {
	db, repo, fakeNow := openQuoteTestRepo(t)
	ctx := context.Background()
	want := sampleSnapshot()
	saved, err := repo.Save(ctx, want, 900*time.Second)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Cleanup(func() { cleanupQuote(t, db, saved.ID) })

	got, err := repo.Get(ctx, saved.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DirectionID != want.DirectionID {
		t.Errorf("DirectionID: got %q, want %q", got.DirectionID, want.DirectionID)
	}
	if got.SourceCountry != want.SourceCountry {
		t.Errorf("SourceCountry: got %q, want %q", got.SourceCountry, want.SourceCountry)
	}
	if got.SourceCurrency != want.SourceCurrency {
		t.Errorf("SourceCurrency: got %q, want %q", got.SourceCurrency, want.SourceCurrency)
	}
	if got.TargetCountry != want.TargetCountry {
		t.Errorf("TargetCountry: got %q, want %q", got.TargetCountry, want.TargetCountry)
	}
	if got.TargetCurrency != want.TargetCurrency {
		t.Errorf("TargetCurrency: got %q, want %q", got.TargetCurrency, want.TargetCurrency)
	}
	if got.PayoutMethod != want.PayoutMethod {
		t.Errorf("PayoutMethod: got %q, want %q", got.PayoutMethod, want.PayoutMethod)
	}
	if got.AmountType != want.AmountType {
		t.Errorf("AmountType: got %q, want %q", got.AmountType, want.AmountType)
	}
	if got.SourceAmountMinor != want.SourceAmountMinor {
		t.Errorf("SourceAmountMinor: got %d, want %d", got.SourceAmountMinor, want.SourceAmountMinor)
	}
	if got.FeeAmountMinor != want.FeeAmountMinor {
		t.Errorf("FeeAmountMinor: got %d, want %d", got.FeeAmountMinor, want.FeeAmountMinor)
	}
	if got.FeeCurrency != want.FeeCurrency {
		t.Errorf("FeeCurrency: got %q, want %q", got.FeeCurrency, want.FeeCurrency)
	}
	if got.AmountToConvertMinor != want.AmountToConvertMinor {
		t.Errorf("AmountToConvertMinor: got %d, want %d", got.AmountToConvertMinor, want.AmountToConvertMinor)
	}
	if !got.Rate.Equal(want.Rate) {
		t.Errorf("Rate: got %s, want %s", got.Rate, want.Rate)
	}
	if got.TargetAmountMinor != want.TargetAmountMinor {
		t.Errorf("TargetAmountMinor: got %d, want %d", got.TargetAmountMinor, want.TargetAmountMinor)
	}
	if !got.ExpiresAt.Equal(fakeNow.Add(900 * time.Second)) {
		t.Errorf("ExpiresAt: got %v, want %v", got.ExpiresAt, fakeNow.Add(900*time.Second))
	}
	if !got.CreatedAt.Equal(fakeNow) {
		t.Errorf("CreatedAt: got %v, want %v", got.CreatedAt, fakeNow)
	}
	var gotItems, wantItems []string
	if err := json.Unmarshal(got.Breakdown, &gotItems); err != nil {
		t.Fatalf("breakdown unmarshal got: %v", err)
	}
	if err := json.Unmarshal(want.Breakdown, &wantItems); err != nil {
		t.Fatalf("breakdown unmarshal want: %v", err)
	}
	if len(gotItems) != len(wantItems) {
		t.Fatalf("breakdown len: got %d, want %d", len(gotItems), len(wantItems))
	}
	for i := range wantItems {
		if gotItems[i] != wantItems[i] {
			t.Errorf("breakdown[%d]: got %q, want %q", i, gotItems[i], wantItems[i])
		}
	}
}

func TestGet_unknownIDReturnsErrNotFound(t *testing.T) {
	_, repo, _ := openQuoteTestRepo(t)
	ctx := context.Background()
	_, err := repo.Get(ctx, "00000000-0000-4000-8000-000000000001")
	if err == nil {
		t.Fatal("Get: want error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: got %v, want ErrNotFound", err)
	}
}

func TestGet_expiredQuoteStillReturned(t *testing.T) {
	db, repo, fakeNow := openQuoteTestRepo(t)
	ctx := context.Background()
	const ttl = 900 * time.Second
	saved, err := repo.Save(ctx, sampleSnapshot(), ttl)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Cleanup(func() { cleanupQuote(t, db, saved.ID) })

	later := saved.ExpiresAt.Add(time.Second)
	if !saved.Expired(later) {
		t.Fatalf("Expired(later): want true at %v", later)
	}
	got, err := repo.Get(ctx, saved.ID)
	if err != nil {
		t.Fatalf("Get after expiry moment: %v", err)
	}
	if got.ID != saved.ID {
		t.Errorf("ID: got %q, want %q", got.ID, saved.ID)
	}
	if got.SourceAmountMinor != saved.SourceAmountMinor {
		t.Errorf("SourceAmountMinor: got %d, want %d", got.SourceAmountMinor, saved.SourceAmountMinor)
	}
	if got.TargetAmountMinor != saved.TargetAmountMinor {
		t.Errorf("TargetAmountMinor: got %d, want %d", got.TargetAmountMinor, saved.TargetAmountMinor)
	}
	if !got.CreatedAt.Equal(fakeNow) {
		t.Errorf("CreatedAt: got %v, want %v", got.CreatedAt, fakeNow)
	}
}
