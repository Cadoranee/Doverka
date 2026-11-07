package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrReused     = errors.New("idempotency key reused")
	ErrInProgress = errors.New("idempotency request in progress")
	ErrNotClaimed = errors.New("idempotency key is not claimed")
)

type Snapshot struct {
	StatusCode int
	Body       json.RawMessage
}

type Decision struct {
	Claimed bool
	Replay  *Snapshot
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Begin(ctx context.Context, tx pgx.Tx, key string, body []byte) (Decision, error) {
	if err := requireStore(s); err != nil {
		return Decision{}, err
	}
	if err := requireTx(tx); err != nil {
		return Decision{}, err
	}
	if key == "" {
		return Decision{}, errors.New("idempotency key is empty")
	}

	hash := hashBody(body)
	tag, err := tx.Exec(ctx, `
INSERT INTO idempotency_keys (idempotency_key, request_hash)
VALUES ($1, $2)
ON CONFLICT (idempotency_key) DO NOTHING`,
		key, hash,
	)
	if err != nil {
		return Decision{}, fmt.Errorf("insert idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return Decision{Claimed: true}, nil
	}

	var storedHash string
	var responseBody []byte
	var statusCode *int
	err = tx.QueryRow(ctx, `
SELECT request_hash, response_body, status_code
FROM idempotency_keys
WHERE idempotency_key = $1
FOR UPDATE`,
		key,
	).Scan(&storedHash, &responseBody, &statusCode)
	if err != nil {
		return Decision{}, fmt.Errorf("load idempotency key: %w", err)
	}
	if storedHash != hash {
		return Decision{}, ErrReused
	}
	if statusCode == nil {
		return Decision{}, ErrInProgress
	}
	if responseBody == nil {
		return Decision{}, errors.New("idempotency snapshot is incomplete")
	}
	return Decision{
		Replay: &Snapshot{
			StatusCode: *statusCode,
			Body:       append(json.RawMessage(nil), responseBody...),
		},
	}, nil
}

func (s *Store) Save(ctx context.Context, tx pgx.Tx, key string, statusCode int, body []byte) error {
	if err := requireStore(s); err != nil {
		return err
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	if key == "" {
		return errors.New("idempotency key is empty")
	}
	if statusCode < 100 || statusCode > 599 {
		return errors.New("idempotency status code is invalid")
	}
	if !json.Valid(body) {
		return errors.New("idempotency response is not json")
	}

	tag, err := tx.Exec(ctx, `
UPDATE idempotency_keys
SET response_body = $2::jsonb, status_code = $3
WHERE idempotency_key = $1 AND status_code IS NULL`,
		key, string(body), statusCode,
	)
	if err != nil {
		return fmt.Errorf("save idempotency snapshot: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotClaimed
	}
	return nil
}

func requireStore(s *Store) error {
	if s == nil || s.pool == nil {
		return errors.New("idempotency store is not configured")
	}
	return nil
}

func requireTx(tx pgx.Tx) error {
	if tx == nil {
		return errors.New("transaction is nil")
	}
	return nil
}

func hashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
