package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound          = errors.New("balance not found")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrAlreadySeeded     = errors.New("balance already seeded")
	ErrAmountMismatch    = errors.New("amount does not match reserve")
	ErrHoldNotOpen       = errors.New("hold is not open")
	ErrHoldExists        = errors.New("hold already exists")
)

type Balance struct {
	ClientID  string
	Currency  string
	Available int64
	Reserved  int64
}

type Entry struct {
	Type        string
	AmountMinor int64
	TransferID  string
}

type Ledger struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Ledger {
	return &Ledger{pool: pool}
}

func (l *Ledger) Seed(ctx context.Context, tx pgx.Tx, clientID, currency string, amountMinor int64) error {
	if err := requireLedger(l); err != nil {
		return err
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := validateClientCurrency(clientID, currency); err != nil {
		return err
	}
	if err := validateAmount(amountMinor); err != nil {
		return err
	}

	_, err := tx.Exec(ctx, `
INSERT INTO client_balances (client_id, currency, available, reserved)
VALUES ($1, $2, 0, 0)`,
		clientID, currency,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrAlreadySeeded
		}
		return fmt.Errorf("insert balance: %w", err)
	}
	if err := insertEntry(ctx, tx, clientID, currency, "seed", amountMinor, ""); err != nil {
		return err
	}
	return project(ctx, tx, clientID, currency)
}

func (l *Ledger) Reserve(ctx context.Context, tx pgx.Tx, clientID, currency, transferID string, amountMinor int64) error {
	if err := requireLedger(l); err != nil {
		return err
	}
	if err := prepareHold(tx, clientID, currency, transferID, amountMinor); err != nil {
		return err
	}
	available, _, err := lockBalance(ctx, tx, clientID, currency)
	if err != nil {
		return err
	}
	if available < amountMinor {
		return ErrInsufficientFunds
	}
	if err := requireNoHold(ctx, tx, transferID); err != nil {
		return err
	}
	if err := insertEntry(ctx, tx, clientID, currency, "reserve", amountMinor, transferID); err != nil {
		return err
	}
	return project(ctx, tx, clientID, currency)
}

func (l *Ledger) Release(ctx context.Context, tx pgx.Tx, clientID, currency, transferID string, amountMinor int64) error {
	return l.closeHold(ctx, tx, clientID, currency, transferID, amountMinor, "release")
}

func (l *Ledger) Debit(ctx context.Context, tx pgx.Tx, clientID, currency, transferID string, amountMinor int64) error {
	return l.closeHold(ctx, tx, clientID, currency, transferID, amountMinor, "debit")
}

func (l *Ledger) Balance(ctx context.Context, clientID, currency string) (Balance, error) {
	if err := requireLedger(l); err != nil {
		return Balance{}, err
	}
	if err := validateClientCurrency(clientID, currency); err != nil {
		return Balance{}, err
	}
	return readBalance(ctx, l.pool, clientID, currency)
}

func (l *Ledger) Entries(ctx context.Context, clientID, currency string) ([]Entry, error) {
	if err := requireLedger(l); err != nil {
		return nil, err
	}
	if err := validateClientCurrency(clientID, currency); err != nil {
		return nil, err
	}
	rows, err := l.pool.Query(ctx, `
SELECT entry_type, amount_minor, transfer_id FROM ledger_entries
WHERE client_id = $1 AND currency = $2
ORDER BY id ASC`,
		clientID, currency,
	)
	if err != nil {
		return nil, fmt.Errorf("select entries: %w", err)
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		var e Entry
		var transferID *string
		if err := rows.Scan(&e.Type, &e.AmountMinor, &transferID); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		if transferID != nil {
			e.TransferID = *transferID
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate entries: %w", err)
	}
	return out, nil
}

func (l *Ledger) closeHold(ctx context.Context, tx pgx.Tx, clientID, currency, transferID string, amountMinor int64, entryType string) error {
	if err := requireLedger(l); err != nil {
		return err
	}
	if err := prepareHold(tx, clientID, currency, transferID, amountMinor); err != nil {
		return err
	}
	_, reserved, err := lockBalance(ctx, tx, clientID, currency)
	if err != nil {
		return err
	}
	if err := requireOpenHold(ctx, tx, clientID, currency, transferID, amountMinor); err != nil {
		return err
	}
	if reserved < amountMinor {
		return ErrInsufficientFunds
	}
	if err := insertEntry(ctx, tx, clientID, currency, entryType, amountMinor, transferID); err != nil {
		return err
	}
	return project(ctx, tx, clientID, currency)
}

func requireLedger(l *Ledger) error {
	if l == nil || l.pool == nil {
		return errors.New("ledger is not configured")
	}
	return nil
}

func requireTx(tx pgx.Tx) error {
	if tx == nil {
		return errors.New("transaction is nil")
	}
	return nil
}

func validateClientCurrency(clientID, currency string) error {
	if clientID == "" {
		return errors.New("client id is empty")
	}
	if len(currency) != 3 {
		return errors.New("currency must be 3 characters")
	}
	return nil
}

func validateAmount(amountMinor int64) error {
	if amountMinor <= 0 {
		return errors.New("amount must be positive")
	}
	return nil
}

func prepareHold(tx pgx.Tx, clientID, currency, transferID string, amountMinor int64) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := validateClientCurrency(clientID, currency); err != nil {
		return err
	}
	if transferID == "" {
		return errors.New("transfer id is empty")
	}
	return validateAmount(amountMinor)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type rowQuery interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func lockBalance(ctx context.Context, tx pgx.Tx, clientID, currency string) (int64, int64, error) {
	var available, reserved int64
	err := tx.QueryRow(ctx, `
SELECT available, reserved FROM client_balances
WHERE client_id = $1 AND currency = $2
FOR UPDATE`,
		clientID, currency,
	).Scan(&available, &reserved)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, ErrNotFound
		}
		return 0, 0, fmt.Errorf("lock balance: %w", err)
	}
	return available, reserved, nil
}

func readBalance(ctx context.Context, q rowQuery, clientID, currency string) (Balance, error) {
	var b Balance
	err := q.QueryRow(ctx, `
SELECT available, reserved FROM client_balances
WHERE client_id = $1 AND currency = $2`,
		clientID, currency,
	).Scan(&b.Available, &b.Reserved)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Balance{}, ErrNotFound
		}
		return Balance{}, fmt.Errorf("select balance: %w", err)
	}
	b.ClientID = clientID
	b.Currency = currency
	return b, nil
}

func insertEntry(ctx context.Context, tx pgx.Tx, clientID, currency, entryType string, amountMinor int64, transferID string) error {
	var transfer any
	if transferID != "" {
		transfer = transferID
	}
	_, err := tx.Exec(ctx, `
INSERT INTO ledger_entries (client_id, currency, entry_type, amount_minor, transfer_id)
VALUES ($1, $2, $3, $4, $5)`,
		clientID, currency, entryType, amountMinor, transfer,
	)
	if err != nil {
		return fmt.Errorf("insert %s entry: %w", entryType, err)
	}
	return nil
}

func project(ctx context.Context, tx pgx.Tx, clientID, currency string) error {
	var available, reserved int64
	err := tx.QueryRow(ctx, `
SELECT
	COALESCE(SUM(CASE entry_type
		WHEN 'seed' THEN amount_minor
		WHEN 'release' THEN amount_minor
		WHEN 'reserve' THEN -amount_minor
		ELSE 0 END), 0)::bigint,
	COALESCE(SUM(CASE entry_type
		WHEN 'reserve' THEN amount_minor
		WHEN 'release' THEN -amount_minor
		WHEN 'debit' THEN -amount_minor
		ELSE 0 END), 0)::bigint
FROM ledger_entries
WHERE client_id = $1 AND currency = $2`,
		clientID, currency,
	).Scan(&available, &reserved)
	if err != nil {
		return fmt.Errorf("project ledger: %w", err)
	}
	if available < 0 || reserved < 0 {
		return ErrInsufficientFunds
	}
	tag, err := tx.Exec(ctx, `
UPDATE client_balances SET available = $3, reserved = $4
WHERE client_id = $1 AND currency = $2`,
		clientID, currency, available, reserved,
	)
	if err != nil {
		return fmt.Errorf("update balance: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

type postedEntry struct {
	clientID  string
	currency  string
	entryType string
	amount    int64
}

func transferEntries(ctx context.Context, tx pgx.Tx, transferID string) ([]postedEntry, error) {
	rows, err := tx.Query(ctx, `
SELECT client_id, currency, entry_type, amount_minor FROM ledger_entries
WHERE transfer_id = $1
ORDER BY id ASC`,
		transferID,
	)
	if err != nil {
		return nil, fmt.Errorf("select hold entries: %w", err)
	}
	defer rows.Close()

	var out []postedEntry
	for rows.Next() {
		var e postedEntry
		if err := rows.Scan(&e.clientID, &e.currency, &e.entryType, &e.amount); err != nil {
			return nil, fmt.Errorf("scan hold entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hold entries: %w", err)
	}
	return out, nil
}

func requireNoHold(ctx context.Context, tx pgx.Tx, transferID string) error {
	entries, err := transferEntries(ctx, tx, transferID)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return ErrHoldExists
	}
	return nil
}

func requireOpenHold(ctx context.Context, tx pgx.Tx, clientID, currency, transferID string, amountMinor int64) error {
	entries, err := transferEntries(ctx, tx, transferID)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].entryType != "reserve" || entries[0].clientID != clientID || entries[0].currency != currency {
		return ErrHoldNotOpen
	}
	if entries[0].amount != amountMinor {
		return ErrAmountMismatch
	}
	return nil
}
