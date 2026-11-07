package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func TestCanTransition_matchesStateDiagram(t *testing.T) {
	all := []Status{
		StatusCreated,
		StatusReserved,
		StatusSubmitting,
		StatusProcessing,
		StatusCompleted,
		StatusFailed,
		StatusNeedsReview,
	}
	allowed := []struct{ from, to Status }{
		{StatusCreated, StatusReserved},
		{StatusReserved, StatusSubmitting},
		{StatusSubmitting, StatusSubmitting},
		{StatusSubmitting, StatusProcessing},
		{StatusSubmitting, StatusFailed},
		{StatusSubmitting, StatusNeedsReview},
		{StatusProcessing, StatusCompleted},
		{StatusProcessing, StatusFailed},
		{StatusProcessing, StatusNeedsReview},
	}
	allowedSet := make(map[Status]map[Status]bool)
	for _, e := range allowed {
		if allowedSet[e.from] == nil {
			allowedSet[e.from] = make(map[Status]bool)
		}
		allowedSet[e.from][e.to] = true
	}
	for _, from := range all {
		for _, to := range all {
			want := allowedSet[from][to]
			if got := CanTransition(from, to); got != want {
				t.Fatalf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
	if CanTransition(Status("BOGUS"), StatusReserved) {
		t.Fatal("bogus from should be false")
	}
	if CanTransition(StatusCreated, Status("BOGUS")) {
		t.Fatal("bogus to should be false")
	}
}

func TestStatus_terminal(t *testing.T) {
	terminal := []Status{StatusCompleted, StatusFailed, StatusNeedsReview}
	nonTerminal := []Status{StatusCreated, StatusReserved, StatusSubmitting, StatusProcessing}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Fatalf("%s should be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Fatalf("%s should not be terminal", s)
		}
	}
}

func openTransferTest(t *testing.T) (*store.DB, *Machine, *clock.Fake) {
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
	base := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	fake := clock.NewFake(base)
	return db, New(db.Pool(), fake), fake
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

func newTestID(t *testing.T) string {
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

func newTestTransfer(t *testing.T, db *store.DB) NewTransfer {
	t.Helper()
	quoteID := newTestID(t)
	insertQuote(t, db.Pool(), quoteID)
	return NewTransfer{
		ID:                newTestID(t),
		QuoteID:           quoteID,
		ClientID:          "c_001",
		SourceAmountMinor: 150000,
		SourceCurrency:    "RUB",
		TargetAmountMinor: 54945,
		TargetCurrency:    "THB",
		Recipient:         json.RawMessage(`{"account":"TH1234"}`),
	}
}

func cleanupTransfer(t *testing.T, db *store.DB, nt NewTransfer) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool().Exec(ctx, `DELETE FROM transfer_transitions WHERE transfer_id = $1`, nt.ID); err != nil {
		t.Fatalf("cleanup transfer_transitions: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM transfers WHERE transfer_id = $1`, nt.ID); err != nil {
		t.Fatalf("cleanup transfers: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `DELETE FROM quotes WHERE quote_id = $1`, nt.QuoteID); err != nil {
		t.Fatalf("cleanup quotes: %v", err)
	}
}

func recipientEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var g, w map[string]any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	gb, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wb, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gb) != string(wb) {
		t.Fatalf("recipient: got %s want %s", gb, wb)
	}
}

func assertHistory(t *testing.T, got, want []Transition) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("history len %d want %d: got %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].From != want[i].From || got[i].To != want[i].To || got[i].Reason != want[i].Reason {
			t.Fatalf("[%d]: got %+v want %+v", i, got[i], want[i])
		}
		if !got[i].At.Equal(want[i].At) {
			t.Fatalf("[%d] at: got %v want %v", i, got[i].At, want[i].At)
		}
	}
}

func TestCreate_startsInCreatedWithHistory(t *testing.T) {
	db, m, fake := openTransferTest(t)
	ctx := context.Background()
	nt := newTestTransfer(t, db)
	t.Cleanup(func() { cleanupTransfer(t, db, nt) })
	base := fake.Now()

	inTx(t, db, func(tx pgx.Tx) error {
		_, err := m.Create(ctx, tx, nt)
		return err
	})

	got, err := m.Get(ctx, nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCreated {
		t.Fatalf("status %s want CREATED", got.Status)
	}
	if got.ID != nt.ID || got.QuoteID != nt.QuoteID || got.ClientID != nt.ClientID {
		t.Fatalf("ids mismatch: %+v", got)
	}
	if got.SourceAmountMinor != nt.SourceAmountMinor || got.TargetAmountMinor != nt.TargetAmountMinor {
		t.Fatalf("amounts mismatch")
	}
	if got.SourceCurrency != nt.SourceCurrency || got.TargetCurrency != nt.TargetCurrency {
		t.Fatalf("currencies mismatch")
	}
	recipientEqual(t, got.Recipient, nt.Recipient)
	if !got.CreatedAt.Equal(base) || !got.UpdatedAt.Equal(base) {
		t.Fatalf("times: created %v updated %v want %v", got.CreatedAt, got.UpdatedAt, base)
	}

	hist, err := m.History(ctx, nt.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	assertHistory(t, hist, []Transition{{From: "", To: StatusCreated, At: base, Reason: "created"}})
}

func TestTransition_happyPathRecordsOrderedHistory(t *testing.T) {
	db, m, fake := openTransferTest(t)
	ctx := context.Background()
	nt := newTestTransfer(t, db)
	t.Cleanup(func() { cleanupTransfer(t, db, nt) })
	base := fake.Now()

	inTx(t, db, func(tx pgx.Tx) error {
		_, err := m.Create(ctx, tx, nt)
		return err
	})

	steps := []struct {
		to     Status
		reason string
	}{
		{StatusReserved, "reserved"},
		{StatusSubmitting, "provider p1"},
		{StatusSubmitting, "provider p2"},
		{StatusProcessing, "payout created"},
		{StatusCompleted, "webhook completed"},
	}
	for _, step := range steps {
		fake.Advance(time.Second)
		inTx(t, db, func(tx pgx.Tx) error {
			_, err := m.Transition(ctx, tx, nt.ID, step.to, step.reason)
			return err
		})
	}

	got, err := m.Get(ctx, nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Fatalf("status %s want COMPLETED", got.Status)
	}
	wantUpdated := base.Add(5 * time.Second)
	if !got.UpdatedAt.Equal(wantUpdated) {
		t.Fatalf("updated_at %v want %v", got.UpdatedAt, wantUpdated)
	}

	wantHist := []Transition{
		{From: "", To: StatusCreated, At: base, Reason: "created"},
		{From: StatusCreated, To: StatusReserved, At: base.Add(time.Second), Reason: "reserved"},
		{From: StatusReserved, To: StatusSubmitting, At: base.Add(2 * time.Second), Reason: "provider p1"},
		{From: StatusSubmitting, To: StatusSubmitting, At: base.Add(3 * time.Second), Reason: "provider p2"},
		{From: StatusSubmitting, To: StatusProcessing, At: base.Add(4 * time.Second), Reason: "payout created"},
		{From: StatusProcessing, To: StatusCompleted, At: base.Add(5 * time.Second), Reason: "webhook completed"},
	}
	hist, err := m.History(ctx, nt.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	assertHistory(t, hist, wantHist)
}

func TestTransition_invalidLeavesStateUnchanged(t *testing.T) {
	db, m, _ := openTransferTest(t)
	ctx := context.Background()
	nt := newTestTransfer(t, db)
	t.Cleanup(func() { cleanupTransfer(t, db, nt) })

	inTx(t, db, func(tx pgx.Tx) error {
		_, err := m.Create(ctx, tx, nt)
		return err
	})

	err := inTxErr(t, db, func(tx pgx.Tx) error {
		_, err := m.Transition(ctx, tx, nt.ID, StatusCompleted, "skip")
		return err
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition got %v", err)
	}

	got, err := m.Get(ctx, nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCreated {
		t.Fatalf("status %s want CREATED", got.Status)
	}
	hist, err := m.History(ctx, nt.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history len %d want 1", len(hist))
	}
}

func TestTransition_terminalIsFinal(t *testing.T) {
	db, m, _ := openTransferTest(t)
	ctx := context.Background()
	nt := newTestTransfer(t, db)
	t.Cleanup(func() { cleanupTransfer(t, db, nt) })

	inTx(t, db, func(tx pgx.Tx) error {
		_, err := m.Create(ctx, tx, nt)
		return err
	})
	for _, step := range []struct {
		to     Status
		reason string
	}{
		{StatusReserved, "reserved"},
		{StatusSubmitting, "submit"},
		{StatusFailed, "failed"},
	} {
		inTx(t, db, func(tx pgx.Tx) error {
			_, err := m.Transition(ctx, tx, nt.ID, step.to, step.reason)
			return err
		})
	}

	for _, to := range []Status{StatusCompleted, StatusProcessing, StatusNeedsReview, StatusFailed} {
		err := inTxErr(t, db, func(tx pgx.Tx) error {
			_, err := m.Transition(ctx, tx, nt.ID, to, "late")
			return err
		})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("to %s: want ErrInvalidTransition got %v", to, err)
		}
	}

	got, err := m.Get(ctx, nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("status %s want FAILED", got.Status)
	}
	hist, err := m.History(ctx, nt.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 4 {
		t.Fatalf("history len %d want 4", len(hist))
	}
}

func TestTransition_unknownTransfer(t *testing.T) {
	db, m, _ := openTransferTest(t)
	ctx := context.Background()
	rid := newTestID(t)

	err := inTxErr(t, db, func(tx pgx.Tx) error {
		_, err := m.Transition(ctx, tx, rid, StatusReserved, "x")
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Transition: want ErrNotFound got %v", err)
	}
	_, err = m.Get(ctx, rid)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: want ErrNotFound got %v", err)
	}
	hist, err := m.History(ctx, rid)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if hist == nil {
		t.Fatal("History: want non-nil empty slice")
	}
	if len(hist) != 0 {
		t.Fatalf("History len %d want 0", len(hist))
	}
}

func TestTransition_rollbackDiscardsChange(t *testing.T) {
	db, m, _ := openTransferTest(t)
	ctx := context.Background()
	nt := newTestTransfer(t, db)
	t.Cleanup(func() { cleanupTransfer(t, db, nt) })

	inTx(t, db, func(tx pgx.Tx) error {
		_, err := m.Create(ctx, tx, nt)
		return err
	})

	err := inTxErr(t, db, func(tx pgx.Tx) error {
		_, err := m.Transition(ctx, tx, nt.ID, StatusReserved, "reserved")
		return err
	})
	if err != nil {
		t.Fatalf("Transition in rolled-back tx: %v", err)
	}

	got, err := m.Get(ctx, nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCreated {
		t.Fatalf("status %s want CREATED", got.Status)
	}
	hist, err := m.History(ctx, nt.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history len %d want 1", len(hist))
	}
}

func TestTransition_concurrentTerminalOnlyOneWins(t *testing.T) {
	db, m, _ := openTransferTest(t)
	ctx := context.Background()
	nt := newTestTransfer(t, db)
	t.Cleanup(func() { cleanupTransfer(t, db, nt) })

	inTx(t, db, func(tx pgx.Tx) error {
		_, err := m.Create(ctx, tx, nt)
		return err
	})
	for _, step := range []struct {
		to     Status
		reason string
	}{
		{StatusReserved, "reserved"},
		{StatusSubmitting, "submit"},
		{StatusProcessing, "processing"},
	} {
		inTx(t, db, func(tx pgx.Tx) error {
			_, err := m.Transition(ctx, tx, nt.ID, step.to, step.reason)
			return err
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var mu sync.Mutex
	var okCount, invalidCount int
	var unexpected []error
	var winners []Transition

	run := func(to Status) {
		defer wg.Done()
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			mu.Lock()
			unexpected = append(unexpected, err)
			mu.Unlock()
			return
		}
		tr, err := m.Transition(ctx, tx, nt.ID, to, fmt.Sprintf("to-%s", to))
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
			winners = append(winners, tr)
		case errors.Is(err, ErrInvalidTransition):
			invalidCount++
		default:
			unexpected = append(unexpected, err)
		}
	}
	go run(StatusCompleted)
	go run(StatusFailed)
	wg.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected: %v", unexpected)
	}
	if okCount != 1 || invalidCount != 1 {
		t.Fatalf("outcomes ok=%d invalid=%d want 1 and 1", okCount, invalidCount)
	}

	hist, err := m.History(ctx, nt.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	var fromProcessing []Transition
	for _, h := range hist {
		if h.From == StatusProcessing {
			fromProcessing = append(fromProcessing, h)
		}
	}
	if len(fromProcessing) != 1 {
		t.Fatalf("PROCESSING exits: %d want 1", len(fromProcessing))
	}

	got, err := m.Get(ctx, nt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != fromProcessing[0].To {
		t.Fatalf("status %s want %s", got.Status, fromProcessing[0].To)
	}
}

func TestCreate_duplicateIDReturnsErrAlreadyExists(t *testing.T) {
	db, m, _ := openTransferTest(t)
	ctx := context.Background()
	nt := newTestTransfer(t, db)
	t.Cleanup(func() { cleanupTransfer(t, db, nt) })

	inTx(t, db, func(tx pgx.Tx) error {
		_, err := m.Create(ctx, tx, nt)
		return err
	})

	err := inTxErr(t, db, func(tx pgx.Tx) error {
		_, err := m.Create(ctx, tx, nt)
		return err
	})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists got %v", err)
	}
}

func TestCreate_rejectsInvalidInput(t *testing.T) {
	db, m, _ := openTransferTest(t)
	ctx := context.Background()
	quoteID := newTestID(t)
	insertQuote(t, db.Pool(), quoteID)
	valid := NewTransfer{
		ID:                newTestID(t),
		QuoteID:           quoteID,
		ClientID:          "c_001",
		SourceAmountMinor: 150000,
		SourceCurrency:    "RUB",
		TargetAmountMinor: 54945,
		TargetCurrency:    "THB",
		Recipient:         json.RawMessage(`{"account":"TH1234"}`),
	}
	t.Cleanup(func() { cleanupTransfer(t, db, valid) })

	cases := []struct {
		name string
		nt   NewTransfer
	}{
		{"empty id", func() NewTransfer { nt := valid; nt.ID = ""; return nt }()},
		{"empty quote", func() NewTransfer { nt := valid; nt.QuoteID = ""; return nt }()},
		{"empty client", func() NewTransfer { nt := valid; nt.ClientID = ""; return nt }()},
		{"source zero", func() NewTransfer { nt := valid; nt.SourceAmountMinor = 0; return nt }()},
		{"target negative", func() NewTransfer { nt := valid; nt.TargetAmountMinor = -1; return nt }()},
		{"source currency short", func() NewTransfer { nt := valid; nt.SourceCurrency = "RU"; return nt }()},
		{"target currency long", func() NewTransfer { nt := valid; nt.TargetCurrency = "THBB"; return nt }()},
		{"recipient nil", func() NewTransfer { nt := valid; nt.Recipient = nil; return nt }()},
		{"recipient not json", func() NewTransfer { nt := valid; nt.Recipient = json.RawMessage(`not json`); return nt }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := inTxErr(t, db, func(tx pgx.Tx) error {
				_, err := m.Create(ctx, tx, tc.nt)
				return err
			})
			if err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestCreate_nilMachineNotConfigured(t *testing.T) {
	var m *Machine
	_, err := m.Create(context.Background(), nil, NewTransfer{})
	if err == nil {
		t.Fatal("want error")
	}
	if err.Error() != "transfer machine is not configured" {
		t.Fatalf("got %q", err.Error())
	}
}
