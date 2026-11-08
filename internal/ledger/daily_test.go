package ledger

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/rates"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

var _ Converter = (*rates.Service)(nil)

type testMinorScale map[string]int

func (m testMinorScale) MinorUnits(currency string) (int, bool) {
	u, ok := m[currency]
	return u, ok
}

var standardTestScale = testMinorScale{
	"RUB": 2,
	"USD": 2,
	"GBP": 2,
	"EUR": 2,
	"JPY": 0,
}

type tableConverter struct {
	rates   map[string]decimal.Decimal
	err     error
	calls   int
	panicOn bool
}

func rateKey(from, to string) string {
	return from + "/" + to
}

func (c *tableConverter) Convert(ctx context.Context, amount decimal.Decimal, from, to string) (decimal.Decimal, error) {
	c.calls++
	if c.panicOn {
		return decimal.Zero, fmt.Errorf("converter must not be called")
	}
	if c.err != nil {
		return decimal.Zero, c.err
	}
	r, ok := c.rates[rateKey(from, to)]
	if !ok {
		return decimal.Zero, fmt.Errorf("no rate %s", rateKey(from, to))
	}
	return amount.Mul(r), nil
}

func cleanupDailyUsage(t *testing.T, db *store.DB, clientID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool().Exec(ctx, `DELETE FROM daily_usage WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("cleanup daily_usage: %v", err)
	}
}

func commitCall(t *testing.T, db *store.DB, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	opErr := fn(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return opErr
}

func assertDailyBuckets(t *testing.T, l *Ledger, clientID string, at time.Time, want []DailyBucket) {
	t.Helper()
	ctx := context.Background()
	got, err := l.DailyUsage(ctx, clientID, at)
	if err != nil {
		t.Fatalf("DailyUsage: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("DailyUsage len: got %d, want %d (got=%v want=%v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DailyUsage[%d]: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func rubCharge(clientID string, at time.Time, sourceMinor int64, limitMajor string) DailyCharge {
	return DailyCharge{
		ClientID:          clientID,
		At:                at,
		SourceCurrency:    "RUB",
		SourceAmountMinor: sourceMinor,
		LimitAmount:       decimal.RequireFromString(limitMajor),
		LimitCurrency:     "RUB",
	}
}

func TestReserveDaily_underLimitRecordsGrossSource(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du1")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{panicOn: true, err: errors.New("must not call")}
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 15000000, "300000.00"), standardTestScale, conv)
	})
	if conv.calls != 0 {
		t.Fatalf("converter calls: %d", conv.calls)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "RUB", AmountMinor: 15000000}})
}

func TestReserveDaily_equalToLimitIsAllowed(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du2")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{}
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 15000000, "300000.00"), standardTestScale, conv)
	})
	err := inTxCommitOnSuccess(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 15000000, "300000.00"), standardTestScale, conv)
	})
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "RUB", AmountMinor: 30000000}})

	err = commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 1, "300000.00"), standardTestScale, conv)
	})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("third reserve: got %v, want ErrDailyLimitExceeded", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "RUB", AmountMinor: 30000000}})
}

func TestReserveDaily_failedTransferFreesBucket(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du3")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{}
	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 20000000, "300000.00"), standardTestScale, conv); err != nil {
			return err
		}
		return l.ReleaseDaily(ctx, tx, clientID, at, "RUB", 20000000)
	})
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{})
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 30000000, "300000.00"), standardTestScale, conv)
	})
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "RUB", AmountMinor: 30000000}})
}

func TestReleaseDaily_moreThanStoredIsError(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du4")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{}
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 100, "300000.00"), standardTestScale, conv)
	})
	err := commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReleaseDaily(ctx, tx, clientID, at, "RUB", 101)
	})
	if err == nil {
		t.Fatal("expected release error")
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "RUB", AmountMinor: 100}})
}

func TestReserveDaily_utcDayBucket(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du5")
	conv := &tableConverter{}
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	day8 := time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)
	day9 := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	mskLate := time.Date(2026, 11, 9, 2, 0, 0, 0, time.FixedZone("MSK", 3*3600))

	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, day8, 10000, "100.00"), standardTestScale, conv)
	})
	err := inTxCommitOnSuccess(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, day9, 10000, "100.00"), standardTestScale, conv)
	})
	if err != nil {
		t.Fatalf("day9 reserve: %v", err)
	}
	err = commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, mskLate, 10000, "100.00"), standardTestScale, conv)
	})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("msk same UTC day: got %v", err)
	}
	assertDailyBuckets(t, l, clientID, day8, []DailyBucket{{Currency: "RUB", AmountMinor: 10000}})
	assertDailyBuckets(t, l, clientID, day9, []DailyBucket{{Currency: "RUB", AmountMinor: 10000}})
}

func TestReserveDaily_convertsOtherLimitCurrency(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du6")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{rates: map[string]decimal.Decimal{
		rateKey("EUR", "USD"): decimal.RequireFromString("1.10"),
		rateKey("GBP", "USD"): decimal.RequireFromString("1.20"),
	}}

	eurCharge := DailyCharge{
		ClientID: clientID, At: at, SourceCurrency: "EUR", SourceAmountMinor: 5000,
		LimitAmount: decimal.RequireFromString("100.00"), LimitCurrency: "USD",
	}
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, eurCharge, standardTestScale, conv)
	})

	gbpFail := DailyCharge{
		ClientID: clientID, At: at, SourceCurrency: "GBP", SourceAmountMinor: 4000,
		LimitAmount: decimal.RequireFromString("100.00"), LimitCurrency: "USD",
	}
	err := commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, gbpFail, standardTestScale, conv)
	})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("GBP 4000: got %v", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "EUR", AmountMinor: 5000}})

	gbpOK := DailyCharge{
		ClientID: clientID, At: at, SourceCurrency: "GBP", SourceAmountMinor: 3000,
		LimitAmount: decimal.RequireFromString("100.00"), LimitCurrency: "USD",
	}
	err = inTxCommitOnSuccess(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, gbpOK, standardTestScale, conv)
	})
	if err != nil {
		t.Fatalf("GBP 3000: %v", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{
		{Currency: "EUR", AmountMinor: 5000},
		{Currency: "GBP", AmountMinor: 3000},
	})
}

func TestReserveDaily_roundsHalfUp(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

	clientOK := newTestClientID(t, "du7a")
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientOK) })
	convOK := &tableConverter{rates: map[string]decimal.Decimal{
		rateKey("JPY", "USD"): decimal.RequireFromString("0.006664"),
	}}
	jpyCharge := DailyCharge{
		ClientID: clientOK, At: at, SourceCurrency: "JPY", SourceAmountMinor: 1000,
		LimitAmount: decimal.RequireFromString("6.66"), LimitCurrency: "USD",
	}
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, jpyCharge, standardTestScale, convOK)
	})
	assertDailyBuckets(t, l, clientOK, at, []DailyBucket{{Currency: "JPY", AmountMinor: 1000}})

	clientFail := newTestClientID(t, "du7b")
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientFail) })
	convFail := &tableConverter{rates: map[string]decimal.Decimal{
		rateKey("JPY", "USD"): decimal.RequireFromString("0.006665"),
	}}
	jpyCharge.ClientID = clientFail
	err := commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, jpyCharge, standardTestScale, convFail)
	})
	if !errors.Is(err, ErrDailyLimitExceeded) {
		t.Fatalf("second client: got %v", err)
	}
	assertDailyBuckets(t, l, clientFail, at, []DailyBucket{})
}

func TestReserveDaily_rateErrorDoesNotWrite(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du8")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	root := errors.New("rate unavailable")
	conv := &tableConverter{err: root}
	ch := DailyCharge{
		ClientID: clientID, At: at, SourceCurrency: "EUR", SourceAmountMinor: 100,
		LimitAmount: decimal.RequireFromString("100.00"), LimitCurrency: "USD",
	}
	err := commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, ch, standardTestScale, conv)
	})
	if !errors.Is(err, root) {
		t.Fatalf("want wrapped rate error, got %v", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{})
}

func TestReserveDaily_concurrent(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du9")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	conv := &tableConverter{}
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	charge := rubCharge(clientID, at, 6000, "100.00")
	tx1, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	open := true
	t.Cleanup(func() {
		if open {
			_ = tx1.Rollback(ctx)
		}
	})
	if err := l.ReserveDaily(ctx, tx1, charge, standardTestScale, conv); err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		tx2, beginErr := db.Pool().Begin(ctx)
		if beginErr != nil {
			done <- beginErr
			return
		}
		opErr := l.ReserveDaily(ctx, tx2, charge, standardTestScale, conv)
		if opErr != nil {
			_ = tx2.Rollback(ctx)
			done <- opErr
			return
		}
		done <- tx2.Commit(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("second reserve finished while the first transaction holds the day: %v", err)
		default:
		}
		var waiting int
		err := db.Pool().QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity
WHERE wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting)
		if err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second reserve did not block on the daily lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit first: %v", err)
	}
	open = false
	select {
	case err := <-done:
		if !errors.Is(err, ErrDailyLimitExceeded) {
			t.Fatalf("second reserve: got %v, want ErrDailyLimitExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second reserve did not finish after the first commit")
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "RUB", AmountMinor: 6000}})
}

func TestReserveDaily_rollbackDoesNotPersist(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "du10")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{}
	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 5000, "100.00"), standardTestScale, conv)
	})
	if err != nil {
		t.Fatalf("Reserve in rolled-back tx: %v", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{})
}

func TestDailyUsage_unknownClientIsEmpty(t *testing.T) {
	_, l := openLedgerTest(t)
	ctx := context.Background()
	got, err := l.DailyUsage(ctx, newTestClientID(t, "unknown"), time.Now().UTC())
	if err != nil {
		t.Fatalf("DailyUsage: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %v, want empty slice", got)
	}
}

func TestReserveDaily_nilLedgerAndNilTx(t *testing.T) {
	ctx := context.Background()
	ch := rubCharge("c", time.Now().UTC(), 1, "1.00")
	var nilLedger *Ledger
	if err := nilLedger.ReserveDaily(ctx, nil, ch, standardTestScale, nil); err == nil || err.Error() != "ledger is not configured" {
		t.Fatalf("nil ledger: got %v", err)
	}
	var empty Ledger
	if err := empty.ReserveDaily(ctx, nil, ch, standardTestScale, nil); err == nil || err.Error() != "ledger is not configured" {
		t.Fatalf("empty ledger: got %v", err)
	}

	_, l := openLedgerTest(t)
	err := l.ReserveDaily(ctx, nil, ch, standardTestScale, nil)
	if err == nil || err.Error() != "transaction is nil" {
		t.Fatalf("nil tx: got %v", err)
	}
}

func TestReleaseDaily_partialLeavesRemainder(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "dupart")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, rubCharge(clientID, at, 100, "300000.00"), standardTestScale, &tableConverter{})
	})
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReleaseDaily(ctx, tx, clientID, at, "RUB", 40)
	})
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "RUB", AmountMinor: 60}})
}

func TestReleaseDaily_missingBucketIsError(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "dumiss")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	err := commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReleaseDaily(ctx, tx, clientID, at, "RUB", 1)
	})
	if err == nil {
		t.Fatal("expected missing bucket error")
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{})
}

func TestReleaseDaily_oneCurrencyLeavesTheOther(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "dufx")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{rates: map[string]decimal.Decimal{
		rateKey("EUR", "USD"): decimal.RequireFromString("1.10"),
		rateKey("GBP", "USD"): decimal.RequireFromString("1.20"),
	}}
	limit := decimal.RequireFromString("100.00")
	inTx(t, db, func(tx pgx.Tx) error {
		eur := DailyCharge{ClientID: clientID, At: at, SourceCurrency: "EUR", SourceAmountMinor: 5000, LimitAmount: limit, LimitCurrency: "USD"}
		if err := l.ReserveDaily(ctx, tx, eur, standardTestScale, conv); err != nil {
			return err
		}
		gbp := DailyCharge{ClientID: clientID, At: at, SourceCurrency: "GBP", SourceAmountMinor: 3000, LimitAmount: limit, LimitCurrency: "USD"}
		return l.ReserveDaily(ctx, tx, gbp, standardTestScale, conv)
	})
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReleaseDaily(ctx, tx, clientID, at, "EUR", 5000)
	})
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{{Currency: "GBP", AmountMinor: 3000}})

	err := inTxCommitOnSuccess(t, db, func(tx pgx.Tx) error {
		eur := DailyCharge{ClientID: clientID, At: at, SourceCurrency: "EUR", SourceAmountMinor: 5000, LimitAmount: limit, LimitCurrency: "USD"}
		return l.ReserveDaily(ctx, tx, eur, standardTestScale, conv)
	})
	if err != nil {
		t.Fatalf("EUR after release: %v", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{
		{Currency: "EUR", AmountMinor: 5000},
		{Currency: "GBP", AmountMinor: 3000},
	})
}

func TestReserveDaily_roundsEachCurrencyBucket(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "durb")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{rates: map[string]decimal.Decimal{
		rateKey("EUR", "USD"): decimal.RequireFromString("0.4"),
		rateKey("GBP", "USD"): decimal.RequireFromString("0.4"),
	}}
	eur := DailyCharge{
		ClientID: clientID, At: at, SourceCurrency: "EUR", SourceAmountMinor: 1,
		LimitAmount: decimal.RequireFromString("0.00"), LimitCurrency: "USD",
	}
	inTx(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, eur, standardTestScale, conv)
	})
	gbp := DailyCharge{
		ClientID: clientID, At: at, SourceCurrency: "GBP", SourceAmountMinor: 1,
		LimitAmount: decimal.RequireFromString("0.00"), LimitCurrency: "USD",
	}
	err := commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, gbp, standardTestScale, conv)
	})
	if err != nil {
		t.Fatalf("second bucket: %v", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{
		{Currency: "EUR", AmountMinor: 1},
		{Currency: "GBP", AmountMinor: 1},
	})
}

func TestReserveDaily_limitOverflowDoesNotWrite(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "duov1")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	ch := DailyCharge{
		ClientID: clientID, At: at, SourceCurrency: "JPY", SourceAmountMinor: 50,
		LimitAmount: decimal.RequireFromString("18446744073709551716"), LimitCurrency: "JPY",
	}
	err := commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, ch, standardTestScale, nil)
	})
	if err == nil || err.Error() != "amount overflow" {
		t.Fatalf("got %v, want amount overflow", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{})
}

func TestReserveDaily_convertedOverflowDoesNotWrite(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "duov2")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(func() { cleanupDailyUsage(t, db, clientID) })

	conv := &tableConverter{rates: map[string]decimal.Decimal{
		rateKey("EUR", "JPY"): decimal.RequireFromString("18446744073709551716"),
	}}
	ch := DailyCharge{
		ClientID: clientID, At: at, SourceCurrency: "EUR", SourceAmountMinor: 100,
		LimitAmount: decimal.RequireFromString("1000"), LimitCurrency: "JPY",
	}
	err := commitCall(t, db, func(tx pgx.Tx) error {
		return l.ReserveDaily(ctx, tx, ch, standardTestScale, conv)
	})
	if err == nil || err.Error() != "amount overflow" {
		t.Fatalf("got %v, want amount overflow", err)
	}
	assertDailyBuckets(t, l, clientID, at, []DailyBucket{})
}
