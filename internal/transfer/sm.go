package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusCreated     Status = "CREATED"
	StatusReserved    Status = "RESERVED"
	StatusSubmitting  Status = "SUBMITTING"
	StatusProcessing  Status = "PROCESSING"
	StatusCompleted   Status = "COMPLETED"
	StatusFailed      Status = "FAILED"
	StatusNeedsReview Status = "NEEDS_REVIEW"
)

var (
	ErrNotFound          = errors.New("transfer not found")
	ErrAlreadyExists     = errors.New("transfer already exists")
	ErrInvalidTransition = errors.New("invalid transfer transition")
)

var allowedTransitions = map[Status]map[Status]bool{
	StatusCreated: {
		StatusReserved: true,
	},
	StatusReserved: {
		StatusSubmitting: true,
	},
	StatusSubmitting: {
		StatusSubmitting:  true,
		StatusProcessing:  true,
		StatusFailed:      true,
		StatusNeedsReview: true,
	},
	StatusProcessing: {
		StatusCompleted:   true,
		StatusFailed:      true,
		StatusNeedsReview: true,
	},
}

func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusNeedsReview:
		return true
	default:
		return false
	}
}

func CanTransition(from, to Status) bool {
	return allowedTransitions[from][to]
}

type NewTransfer struct {
	ID                string
	QuoteID           string
	ClientID          string
	SourceAmountMinor int64
	SourceCurrency    string
	TargetAmountMinor int64
	TargetCurrency    string
	Recipient         json.RawMessage
}

type Transfer struct {
	NewTransfer
	Status    Status
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Transition struct {
	From   Status
	To     Status
	At     time.Time
	Reason string
}

type Machine struct {
	pool *pgxpool.Pool
	clk  clock.Clock
}

func New(pool *pgxpool.Pool, clk clock.Clock) *Machine {
	return &Machine{pool: pool, clk: clk}
}

func (m *Machine) Create(ctx context.Context, tx pgx.Tx, nt NewTransfer) (Transfer, error) {
	if err := requireMachine(m); err != nil {
		return Transfer{}, err
	}
	if err := requireTx(tx); err != nil {
		return Transfer{}, err
	}
	if err := validateNewTransfer(nt); err != nil {
		return Transfer{}, err
	}
	now := m.clk.Now().UTC()
	_, err := tx.Exec(ctx, `
INSERT INTO transfers (
	transfer_id, quote_id, client_id, status,
	source_amount_minor, source_currency, target_amount_minor, target_currency,
	recipient, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11)`,
		nt.ID,
		nt.QuoteID,
		nt.ClientID,
		string(StatusCreated),
		nt.SourceAmountMinor,
		nt.SourceCurrency,
		nt.TargetAmountMinor,
		nt.TargetCurrency,
		string(nt.Recipient),
		now,
		now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return Transfer{}, ErrAlreadyExists
		}
		return Transfer{}, fmt.Errorf("insert transfer: %w", err)
	}
	tr := Transition{From: "", To: StatusCreated, At: now, Reason: "created"}
	if err := insertTransition(ctx, tx, nt.ID, tr); err != nil {
		return Transfer{}, err
	}
	created := nt
	created.Recipient = append(json.RawMessage(nil), nt.Recipient...)
	return Transfer{
		NewTransfer: created,
		Status:      StatusCreated,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (m *Machine) Transition(ctx context.Context, tx pgx.Tx, transferID string, to Status, reason string) (Transition, error) {
	if err := requireMachine(m); err != nil {
		return Transition{}, err
	}
	if err := requireTx(tx); err != nil {
		return Transition{}, err
	}
	if transferID == "" {
		return Transition{}, errors.New("transfer id is empty")
	}
	var fromStr string
	err := tx.QueryRow(ctx, `SELECT status FROM transfers WHERE transfer_id = $1 FOR UPDATE`, transferID).Scan(&fromStr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Transition{}, ErrNotFound
		}
		return Transition{}, fmt.Errorf("lock transfer: %w", err)
	}
	from := Status(fromStr)
	if !CanTransition(from, to) {
		return Transition{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	now := m.clk.Now().UTC()
	_, err = tx.Exec(ctx, `
UPDATE transfers SET status = $2, updated_at = $3 WHERE transfer_id = $1`,
		transferID, string(to), now,
	)
	if err != nil {
		return Transition{}, fmt.Errorf("update transfer status: %w", err)
	}
	tr := Transition{From: from, To: to, At: now, Reason: reason}
	if err := insertTransition(ctx, tx, transferID, tr); err != nil {
		return Transition{}, err
	}
	return tr, nil
}

func (m *Machine) Get(ctx context.Context, transferID string) (Transfer, error) {
	if err := requireMachine(m); err != nil {
		return Transfer{}, err
	}
	if transferID == "" {
		return Transfer{}, ErrNotFound
	}
	var (
		statusStr string
		recipient []byte
	)
	var tr Transfer
	err := m.pool.QueryRow(ctx, `
SELECT transfer_id, quote_id, client_id, status,
	source_amount_minor, source_currency, target_amount_minor, target_currency,
	recipient, created_at, updated_at
FROM transfers WHERE transfer_id = $1`,
		transferID,
	).Scan(
		&tr.ID,
		&tr.QuoteID,
		&tr.ClientID,
		&statusStr,
		&tr.SourceAmountMinor,
		&tr.SourceCurrency,
		&tr.TargetAmountMinor,
		&tr.TargetCurrency,
		&recipient,
		&tr.CreatedAt,
		&tr.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Transfer{}, ErrNotFound
		}
		return Transfer{}, fmt.Errorf("select transfer: %w", err)
	}
	tr.Status = Status(statusStr)
	tr.Recipient = append(json.RawMessage(nil), recipient...)
	tr.CreatedAt = tr.CreatedAt.UTC()
	tr.UpdatedAt = tr.UpdatedAt.UTC()
	return tr, nil
}

func (m *Machine) History(ctx context.Context, transferID string) ([]Transition, error) {
	if err := requireMachine(m); err != nil {
		return nil, err
	}
	rows, err := m.pool.Query(ctx, `
SELECT from_status, to_status, occurred_at, reason
FROM transfer_transitions
WHERE transfer_id = $1
ORDER BY id ASC`,
		transferID,
	)
	if err != nil {
		return nil, fmt.Errorf("select transitions: %w", err)
	}
	defer rows.Close()

	out := []Transition{}
	for rows.Next() {
		var tr Transition
		var fromStr, toStr string
		if err := rows.Scan(&fromStr, &toStr, &tr.At, &tr.Reason); err != nil {
			return nil, fmt.Errorf("scan transition: %w", err)
		}
		tr.From = Status(fromStr)
		tr.To = Status(toStr)
		tr.At = tr.At.UTC()
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transitions: %w", err)
	}
	return out, nil
}

func requireMachine(m *Machine) error {
	if m == nil || m.pool == nil || m.clk == nil {
		return errors.New("transfer machine is not configured")
	}
	return nil
}

func requireTx(tx pgx.Tx) error {
	if tx == nil {
		return errors.New("transaction is nil")
	}
	return nil
}

func validateNewTransfer(nt NewTransfer) error {
	if nt.ID == "" {
		return errors.New("transfer id is empty")
	}
	if nt.QuoteID == "" {
		return errors.New("quote id is empty")
	}
	if nt.ClientID == "" {
		return errors.New("client id is empty")
	}
	if nt.SourceAmountMinor <= 0 {
		return errors.New("source amount must be positive")
	}
	if nt.TargetAmountMinor <= 0 {
		return errors.New("target amount must be positive")
	}
	if len(nt.SourceCurrency) != 3 {
		return errors.New("source currency must be 3 characters")
	}
	if len(nt.TargetCurrency) != 3 {
		return errors.New("target currency must be 3 characters")
	}
	if len(nt.Recipient) == 0 {
		return errors.New("recipient is empty")
	}
	if !json.Valid(nt.Recipient) {
		return errors.New("recipient is not valid json")
	}
	return nil
}

func insertTransition(ctx context.Context, tx pgx.Tx, transferID string, tr Transition) error {
	_, err := tx.Exec(ctx, `
INSERT INTO transfer_transitions (transfer_id, from_status, to_status, occurred_at, reason)
VALUES ($1, $2, $3, $4, $5)`,
		transferID,
		string(tr.From),
		string(tr.To),
		tr.At,
		tr.Reason,
	)
	if err != nil {
		return fmt.Errorf("insert transition: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
