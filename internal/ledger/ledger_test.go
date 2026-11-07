package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func openLedgerTest(t *testing.T) (*store.DB, *Ledger) {
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
	return db, New(db.Pool())
}

func inTx(t *testing.T, db *store.DB, fn func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("%v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func inTxErr(t *testing.T, db *store.DB, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	opErr := fn(tx)
	_ = tx.Rollback(ctx)
	return opErr
}

func inTxCommitOnSuccess(t *testing.T, db *store.DB, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if opErr := fn(tx); opErr != nil {
		_ = tx.Rollback(ctx)
		return opErr
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return nil
}

func newTestClientID(t *testing.T, label string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return fmt.Sprintf("%s-%s", label, hex.EncodeToString(b[:]))
}

func newTestTransferID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func insertQuote(t *testing.T, exec execer, quoteID string) {
	t.Helper()
	ctx := context.Background()
	expires := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	_, err := exec.Exec(ctx, `
INSERT INTO quotes (
	quote_id, direction_id, source_country, source_currency, target_country, target_currency,
	payout_method, amount_type, source_amount_minor, fee_amount_minor, fee_currency,
	amount_to_convert_minor, rate, target_amount_minor, breakdown, expires_at, created_at
) VALUES (
	$1, 'RU-TH-BANK', 'RU', 'RUB', 'TH', 'THB', 'BANK_ACCOUNT', 'SOURCE',
	150000, 1500, 'RUB', 148500, $2::numeric, 54945, '[]'::jsonb, $3, $3
)`,
		quoteID,
		"0.3700",
		expires,
	)
	if err != nil {
		t.Fatalf("insert quote: %v", err)
	}
}

func insertTransfer(t *testing.T, exec execer, transferID, quoteID, clientID, sourceCurrency, targetCurrency string, sourceAmount, targetAmount int64) {
	t.Helper()
	ctx := context.Background()
	_, err := exec.Exec(ctx, `
INSERT INTO transfers (
	transfer_id, quote_id, client_id, status,
	source_amount_minor, source_currency, target_amount_minor, target_currency, recipient
) VALUES ($1, $2, $3, 'CREATED', $4, $5, $6, $7, '{}'::jsonb)`,
		transferID, quoteID, clientID, sourceAmount, sourceCurrency, targetAmount, targetCurrency,
	)
	if err != nil {
		t.Fatalf("insert transfer: %v", err)
	}
}

func ensureTransfer(t *testing.T, exec execer, clientID, transferID, sourceCurrency, targetCurrency string, sourceAmount, targetAmount int64) {
	t.Helper()
	quoteID := newTestTransferID(t)
	insertQuote(t, exec, quoteID)
	insertTransfer(t, exec, transferID, quoteID, clientID, sourceCurrency, targetCurrency, sourceAmount, targetAmount)
}

func cleanupClient(t *testing.T, db *store.DB, clientID string) {
	t.Helper()
	if clientID == "" {
		return
	}
	ctx := context.Background()
	rows, err := db.Pool().Query(ctx, `SELECT quote_id FROM transfers WHERE client_id = $1`, clientID)
	if err != nil {
		t.Fatalf("list quotes: %v", err)
	}
	var quoteIDs []string
	for rows.Next() {
		var qid string
		if err := rows.Scan(&qid); err != nil {
			rows.Close()
			t.Fatalf("scan quote_id: %v", err)
		}
		quoteIDs = append(quoteIDs, qid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate quote_id: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM ledger_entries WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("cleanup ledger_entries: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM transfers WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("cleanup transfers: %v", err)
	}
	for _, qid := range quoteIDs {
		if _, err := db.Pool().Exec(ctx, `DELETE FROM quotes WHERE quote_id = $1`, qid); err != nil {
			t.Fatalf("cleanup quotes: %v", err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM client_balances WHERE client_id = $1`, clientID); err != nil {
		t.Fatalf("cleanup client_balances: %v", err)
	}
}

func assertBalance(t *testing.T, l *Ledger, clientID, currency string, wantAvail, wantReserved int64) {
	t.Helper()
	ctx := context.Background()
	b, err := l.Balance(ctx, clientID, currency)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if b.Available != wantAvail || b.Reserved != wantReserved {
		t.Fatalf("balance: got available %d reserved %d, want %d and %d", b.Available, b.Reserved, wantAvail, wantReserved)
	}
}

func assertEntries(t *testing.T, l *Ledger, clientID, currency string, want []Entry) {
	t.Helper()
	ctx := context.Background()
	got, err := l.Entries(ctx, clientID, currency)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("entry count: got %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry[%d]: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSeed_nilLedgerNotConfigured(t *testing.T) {
	ctx := context.Background()
	var l *Ledger
	err := l.Seed(ctx, nil, "c", "RUB", 1)
	if err == nil || err.Error() != "ledger is not configured" {
		t.Fatalf("Seed: got %v", err)
	}
}

func TestSeed_setsAvailableAndOneEntry(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "seed1")
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100000)
	})
	assertBalance(t, l, clientID, "RUB", 100000, 0)
	assertEntries(t, l, clientID, "RUB", []Entry{{Type: "seed", AmountMinor: 100000}})
}

func TestSeed_secondReturnsErrAlreadySeeded(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "seed2")
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100000)
	})
	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 200000)
	})
	if !errors.Is(err, ErrAlreadySeeded) {
		t.Fatalf("Seed second: got %v, want ErrAlreadySeeded", err)
	}
	assertBalance(t, l, clientID, "RUB", 100000, 0)
	assertEntries(t, l, clientID, "RUB", []Entry{{Type: "seed", AmountMinor: 100000}})
}

func TestSeed_nonPositiveAmountReturnsError(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	c0 := newTestClientID(t, "seed0")
	cNeg := newTestClientID(t, "seed-1")
	t.Cleanup(func() { cleanupClient(t, db, c0) })
	t.Cleanup(func() { cleanupClient(t, db, cNeg) })

	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, c0, "RUB", 0)
	})
	if err == nil {
		t.Fatal("Seed 0: want error")
	}
	err = inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, cNeg, "RUB", -1)
	})
	if err == nil {
		t.Fatal("Seed -1: want error")
	}
	if _, err := l.Balance(ctx, c0, "RUB"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Balance after 0 seed: got %v, want ErrNotFound", err)
	}
	if _, err := l.Balance(ctx, cNeg, "RUB"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Balance after -1 seed: got %v, want ErrNotFound", err)
	}
}

func TestReserve_afterSeedUpdatesBalance(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "res1")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100000)
	})
	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 15000, 5000)
	inTx(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, clientID, "RUB", transferID, 15000)
	})
	assertBalance(t, l, clientID, "RUB", 85000, 15000)
	assertEntries(t, l, clientID, "RUB", []Entry{
		{Type: "seed", AmountMinor: 100000},
		{Type: "reserve", AmountMinor: 15000, TransferID: transferID},
	})
}

func TestReserve_insufficientFunds(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "res2")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 1000)
	})
	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 1001, 300)
	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, clientID, "RUB", transferID, 1001)
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("Reserve: got %v, want ErrInsufficientFunds", err)
	}
	assertBalance(t, l, clientID, "RUB", 1000, 0)
	assertEntries(t, l, clientID, "RUB", []Entry{{Type: "seed", AmountMinor: 1000}})
}

func TestReserve_withoutSeedReturnsErrNotFound(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "res3")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 100, 50)
	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, clientID, "RUB", transferID, 100)
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reserve: got %v, want ErrNotFound", err)
	}
}

func TestRelease_restoresAvailable(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "rel1")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100000)
	})
	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 15000, 5000)
	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.Reserve(ctx, tx, clientID, "RUB", transferID, 15000); err != nil {
			return err
		}
		return l.Release(ctx, tx, clientID, "RUB", transferID, 15000)
	})
	assertBalance(t, l, clientID, "RUB", 100000, 0)
	assertEntries(t, l, clientID, "RUB", []Entry{
		{Type: "seed", AmountMinor: 100000},
		{Type: "reserve", AmountMinor: 15000, TransferID: transferID},
		{Type: "release", AmountMinor: 15000, TransferID: transferID},
	})
}

func TestDebit_keepsAvailableAndInvariant(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "deb1")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100000)
	})
	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 15000, 5000)
	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.Reserve(ctx, tx, clientID, "RUB", transferID, 15000); err != nil {
			return err
		}
		return l.Debit(ctx, tx, clientID, "RUB", transferID, 15000)
	})
	assertBalance(t, l, clientID, "RUB", 85000, 0)
	assertEntries(t, l, clientID, "RUB", []Entry{
		{Type: "seed", AmountMinor: 100000},
		{Type: "reserve", AmountMinor: 15000, TransferID: transferID},
		{Type: "debit", AmountMinor: 15000, TransferID: transferID},
	})
}

func TestHold_transitions(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()

	type holdOp func(tx pgx.Tx, clientID, transferID string) error
	ops := map[string]holdOp{
		"reserve": func(tx pgx.Tx, c, id string) error { return l.Reserve(ctx, tx, c, "RUB", id, 15000) },
		"release": func(tx pgx.Tx, c, id string) error { return l.Release(ctx, tx, c, "RUB", id, 15000) },
		"debit":   func(tx pgx.Tx, c, id string) error { return l.Debit(ctx, tx, c, "RUB", id, 15000) },
	}
	setups := map[string][]string{
		"none":     nil,
		"open":     {"reserve"},
		"released": {"reserve", "release"},
		"debited":  {"reserve", "debit"},
	}
	cases := []struct {
		from, op     string
		wantErr      error
		wantAvail    int64
		wantReserved int64
	}{
		{"none", "reserve", nil, 85000, 15000},
		{"none", "release", ErrHoldNotOpen, 100000, 0},
		{"none", "debit", ErrHoldNotOpen, 100000, 0},
		{"open", "reserve", ErrHoldExists, 85000, 15000},
		{"open", "release", nil, 100000, 0},
		{"open", "debit", nil, 85000, 0},
		{"released", "reserve", ErrHoldExists, 100000, 0},
		{"released", "release", ErrHoldNotOpen, 100000, 0},
		{"released", "debit", ErrHoldNotOpen, 100000, 0},
		{"debited", "reserve", ErrHoldExists, 85000, 0},
		{"debited", "release", ErrHoldNotOpen, 85000, 0},
		{"debited", "debit", ErrHoldNotOpen, 85000, 0},
	}

	for _, tc := range cases {
		t.Run(tc.from+"_"+tc.op, func(t *testing.T) {
			clientID := newTestClientID(t, "hold")
			transferID := newTestTransferID(t)
			t.Cleanup(func() { cleanupClient(t, db, clientID) })

			inTx(t, db, func(tx pgx.Tx) error {
				return l.Seed(ctx, tx, clientID, "RUB", 100000)
			})
			ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 15000, 5000)
			for _, step := range setups[tc.from] {
				inTx(t, db, func(tx pgx.Tx) error { return ops[step](tx, clientID, transferID) })
			}

			err := inTxCommitOnSuccess(t, db, func(tx pgx.Tx) error {
				return ops[tc.op](tx, clientID, transferID)
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("%s from %s: got %v, want %v", tc.op, tc.from, err, tc.wantErr)
			}
			assertBalance(t, l, clientID, "RUB", tc.wantAvail, tc.wantReserved)
		})
	}
}

func TestRelease_holdOfAnotherOwnerReturnsErrHoldNotOpen(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	owner := newTestClientID(t, "owner")
	other := newTestClientID(t, "other")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, owner) })
	t.Cleanup(func() { cleanupClient(t, db, other) })

	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.Seed(ctx, tx, owner, "RUB", 100000); err != nil {
			return err
		}
		if err := l.Seed(ctx, tx, owner, "KWD", 3000); err != nil {
			return err
		}
		return l.Seed(ctx, tx, other, "RUB", 100000)
	})
	ensureTransfer(t, db.Pool(), owner, transferID, "RUB", "THB", 15000, 5000)
	inTx(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, owner, "RUB", transferID, 15000)
	})

	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Release(ctx, tx, other, "RUB", transferID, 15000)
	})
	if !errors.Is(err, ErrHoldNotOpen) {
		t.Fatalf("Release by other client: got %v, want ErrHoldNotOpen", err)
	}
	err = inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Release(ctx, tx, owner, "KWD", transferID, 15000)
	})
	if !errors.Is(err, ErrHoldNotOpen) {
		t.Fatalf("Release in other currency: got %v, want ErrHoldNotOpen", err)
	}
	assertBalance(t, l, owner, "RUB", 85000, 15000)
	assertBalance(t, l, owner, "KWD", 3000, 0)
	assertBalance(t, l, other, "RUB", 100000, 0)
}

func TestRelease_amountMismatch(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "relmm")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100000)
	})
	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 15000, 5000)
	inTx(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, clientID, "RUB", transferID, 15000)
	})
	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Release(ctx, tx, clientID, "RUB", transferID, 14999)
	})
	if !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("Release: got %v, want ErrAmountMismatch", err)
	}
	assertBalance(t, l, clientID, "RUB", 85000, 15000)
}

func TestDebit_amountMismatch(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "debmm")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100000)
	})
	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 15000, 5000)
	inTx(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, clientID, "RUB", transferID, 15000)
	})
	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Debit(ctx, tx, clientID, "RUB", transferID, 14000)
	})
	if !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("Debit: got %v, want ErrAmountMismatch", err)
	}
	assertBalance(t, l, clientID, "RUB", 85000, 15000)
}

func TestMixed_invariant(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "mix")
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 500000)
	})

	transferA := newTestTransferID(t)
	transferB := newTestTransferID(t)
	transferC := newTestTransferID(t)
	ensureTransfer(t, db.Pool(), clientID, transferA, "RUB", "THB", 100000, 30000)
	ensureTransfer(t, db.Pool(), clientID, transferB, "RUB", "THB", 40000, 12000)
	ensureTransfer(t, db.Pool(), clientID, transferC, "RUB", "THB", 25000, 8000)

	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.Reserve(ctx, tx, clientID, "RUB", transferA, 100000); err != nil {
			return err
		}
		if err := l.Debit(ctx, tx, clientID, "RUB", transferA, 100000); err != nil {
			return err
		}
		if err := l.Reserve(ctx, tx, clientID, "RUB", transferB, 40000); err != nil {
			return err
		}
		if err := l.Release(ctx, tx, clientID, "RUB", transferB, 40000); err != nil {
			return err
		}
		return l.Reserve(ctx, tx, clientID, "RUB", transferC, 25000)
	})

	assertBalance(t, l, clientID, "RUB", 375000, 25000)
	assertEntries(t, l, clientID, "RUB", []Entry{
		{Type: "seed", AmountMinor: 500000},
		{Type: "reserve", AmountMinor: 100000, TransferID: transferA},
		{Type: "debit", AmountMinor: 100000, TransferID: transferA},
		{Type: "reserve", AmountMinor: 40000, TransferID: transferB},
		{Type: "release", AmountMinor: 40000, TransferID: transferB},
		{Type: "reserve", AmountMinor: 25000, TransferID: transferC},
	})
}

func TestSeed_multipleCurrencies(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "multicur")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.Seed(ctx, tx, clientID, "RUB", 100000); err != nil {
			return err
		}
		return l.Seed(ctx, tx, clientID, "KWD", 3000)
	})
	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 10000, 3000)
	inTx(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, clientID, "RUB", transferID, 10000)
	})
	assertBalance(t, l, clientID, "RUB", 90000, 10000)
	assertBalance(t, l, clientID, "KWD", 3000, 0)
}

func TestReserve_doesNotAffectOtherClient(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientA := newTestClientID(t, "cliA")
	clientB := newTestClientID(t, "cliB")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientA) })
	t.Cleanup(func() { cleanupClient(t, db, clientB) })

	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.Seed(ctx, tx, clientA, "RUB", 50000); err != nil {
			return err
		}
		return l.Seed(ctx, tx, clientB, "RUB", 80000)
	})
	ensureTransfer(t, db.Pool(), clientA, transferID, "RUB", "THB", 10000, 3000)
	inTx(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, clientA, "RUB", transferID, 10000)
	})
	assertBalance(t, l, clientA, "RUB", 40000, 10000)
	assertBalance(t, l, clientB, "RUB", 80000, 0)
}

func TestReserve_concurrent(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "conc")
	transfer1 := newTestTransferID(t)
	transfer2 := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100)
	})
	ensureTransfer(t, db.Pool(), clientID, transfer1, "RUB", "THB", 80, 20)
	ensureTransfer(t, db.Pool(), clientID, transfer2, "RUB", "THB", 80, 20)

	var wg sync.WaitGroup
	wg.Add(2)
	var mu sync.Mutex
	var okCount, insufficientCount int
	var unexpected []error

	run := func(transferID string) {
		defer wg.Done()
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			mu.Lock()
			unexpected = append(unexpected, err)
			mu.Unlock()
			return
		}
		err = l.Reserve(ctx, tx, clientID, "RUB", transferID, 80)
		if err != nil {
			_ = tx.Rollback(ctx)
		} else if commitErr := tx.Commit(ctx); commitErr != nil {
			err = commitErr
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, ErrInsufficientFunds):
			insufficientCount++
		default:
			unexpected = append(unexpected, err)
		}
	}
	go run(transfer1)
	go run(transfer2)
	wg.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected: %v", unexpected)
	}
	if okCount != 1 || insufficientCount != 1 {
		t.Fatalf("outcomes: ok=%d insufficient=%d, want 1 and 1", okCount, insufficientCount)
	}
	assertBalance(t, l, clientID, "RUB", 20, 80)
}

func TestReserve_rollbackDropsHold(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "rb")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		return l.Seed(ctx, tx, clientID, "RUB", 100000)
	})
	ensureTransfer(t, db.Pool(), clientID, transferID, "RUB", "THB", 15000, 5000)
	err := inTxErr(t, db, func(tx pgx.Tx) error {
		return l.Reserve(ctx, tx, clientID, "RUB", transferID, 15000)
	})
	if err != nil {
		t.Fatalf("Reserve inside rolled-back tx: %v", err)
	}
	assertBalance(t, l, clientID, "RUB", 100000, 0)
	assertEntries(t, l, clientID, "RUB", []Entry{{Type: "seed", AmountMinor: 100000}})
}

func TestReserve_sameTransactionAsTransferInsert(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "sametx")
	transferID := newTestTransferID(t)
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.Seed(ctx, tx, clientID, "RUB", 100000); err != nil {
			return err
		}
		ensureTransfer(t, tx, clientID, transferID, "RUB", "THB", 15000, 5000)
		return l.Reserve(ctx, tx, clientID, "RUB", transferID, 15000)
	})
	assertBalance(t, l, clientID, "RUB", 85000, 15000)
}

func TestList_returnsCurrenciesOrdered(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "list-order")
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	inTx(t, db, func(tx pgx.Tx) error {
		if err := l.Seed(ctx, tx, clientID, "RUB", 100000); err != nil {
			return err
		}
		return l.Seed(ctx, tx, clientID, "KWD", 3000)
	})

	got, err := l.List(ctx, clientID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].Currency != "KWD" || got[0].Available != 3000 || got[0].Reserved != 0 || got[0].ClientID != clientID {
		t.Fatalf("KWD: %+v", got[0])
	}
	if got[1].Currency != "RUB" || got[1].Available != 100000 || got[1].Reserved != 0 || got[1].ClientID != clientID {
		t.Fatalf("RUB: %+v", got[1])
	}
}

func TestList_unknownClientIsEmpty(t *testing.T) {
	db, l := openLedgerTest(t)
	ctx := context.Background()
	clientID := newTestClientID(t, "list-empty")
	t.Cleanup(func() { cleanupClient(t, db, clientID) })

	got, err := l.List(ctx, clientID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatal("got nil slice, want non-nil empty")
	}
	if len(got) != 0 {
		t.Fatalf("len=%d, want 0", len(got))
	}
}
