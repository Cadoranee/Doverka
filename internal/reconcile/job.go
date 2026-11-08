package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/ledger"
	providerclient "github.com/Cadoranee/Doverka/internal/provider/client"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	Interval   = 30 * time.Second
	StaleAfter = 2 * time.Minute
)

type PayoutReader interface {
	GetPayout(ctx context.Context, providerID, payoutID string) (providerclient.Payout, bool, error)
}

type Job struct {
	pool       *pgxpool.Pool
	transfers  *transfer.Machine
	balances   *ledger.Ledger
	payouts    PayoutReader
	clk        clock.Clock
	interval   time.Duration
	staleAfter time.Duration
}

func NewJob(pool *pgxpool.Pool, transfers *transfer.Machine, balances *ledger.Ledger, payouts PayoutReader, clk clock.Clock) *Job {
	return &Job{
		pool:       pool,
		transfers:  transfers,
		balances:   balances,
		payouts:    payouts,
		clk:        clk,
		interval:   Interval,
		staleAfter: StaleAfter,
	}
}

func (j *Job) configured() error {
	if j == nil || j.pool == nil || j.transfers == nil || j.balances == nil || j.payouts == nil || j.clk == nil {
		return errors.New("reconcile job is not configured")
	}
	if j.interval <= 0 || j.staleAfter <= 0 {
		return errors.New("reconcile job is not configured")
	}
	return nil
}

func (j *Job) RunOnce(ctx context.Context) error {
	if err := j.configured(); err != nil {
		return err
	}

	ids, err := j.listStale(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := j.reconcileOne(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("reconcile %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (j *Job) Run(ctx context.Context) error {
	if err := j.configured(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()

	for {
		_ = j.RunOnce(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (j *Job) listStale(ctx context.Context) ([]string, error) {
	cutoff := j.clk.Now().UTC().Add(-j.staleAfter)
	rows, err := j.pool.Query(ctx, `
SELECT transfer_id FROM transfers
WHERE status = $1 AND updated_at < $2
ORDER BY updated_at, transfer_id`,
		string(transfer.StatusProcessing), cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("list stale processing transfers: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list stale processing transfers: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stale processing transfers: %w", err)
	}
	return ids, nil
}

func (j *Job) reconcileOne(ctx context.Context, transferID string) error {
	tr, err := j.transfers.Get(ctx, transferID)
	if err != nil {
		return err
	}
	if tr.Status != transfer.StatusProcessing {
		return nil
	}

	attempts, err := j.transfers.Attempts(ctx, transferID)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		return j.markIndeterminate(ctx, tr)
	}
	last := attempts[len(attempts)-1]
	if last.PayoutID == "" {
		return j.markIndeterminate(ctx, tr)
	}

	payout, found, err := j.payouts.GetPayout(ctx, last.ProviderID, last.PayoutID)
	if err != nil {
		return err
	}
	if !found || payout.PayoutID != last.PayoutID || (payout.Reference != "" && payout.Reference != tr.ID) {
		return j.markIndeterminate(ctx, tr)
	}

	switch payout.Status {
	case "COMPLETED":
		return j.settle(ctx, tr, last.ProviderID, last.PayoutID, transfer.StatusCompleted, "reconcile completed")
	case "FAILED":
		return j.settle(ctx, tr, last.ProviderID, last.PayoutID, transfer.StatusFailed, "reconcile failed")
	case "PROCESSING":
		return nil
	default:
		return j.markIndeterminate(ctx, tr)
	}
}

func (j *Job) settle(ctx context.Context, tr transfer.Transfer, providerID, payoutID string, to transfer.Status, reason string) error {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := j.balances.LockBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
		return err
	}
	current, err := lockTransferStatus(ctx, tx, tr.ID)
	if err != nil {
		return err
	}
	if current != transfer.StatusProcessing {
		return nil
	}
	if err := postHold(ctx, tx, j.balances, tr, to); err != nil {
		return err
	}
	if _, err := j.transfers.Transition(ctx, tx, tr.ID, to, reason); err != nil {
		return err
	}
	if err := markAttemptResult(ctx, tx, tr.ID, providerID, payoutID, string(to)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func postHold(ctx context.Context, tx pgx.Tx, balances *ledger.Ledger, tr transfer.Transfer, to transfer.Status) error {
	switch to {
	case transfer.StatusCompleted:
		return balances.Debit(ctx, tx, tr.ClientID, tr.SourceCurrency, tr.ID, tr.SourceAmountMinor)
	case transfer.StatusFailed:
		if err := balances.Release(ctx, tx, tr.ClientID, tr.SourceCurrency, tr.ID, tr.SourceAmountMinor); err != nil {
			return err
		}
		return balances.ReleaseDailyIfPresent(ctx, tx, tr.ClientID, tr.CreatedAt, tr.SourceCurrency, tr.SourceAmountMinor)
	default:
		return fmt.Errorf("unsupported reconcile status %s", to)
	}
}

func (j *Job) markIndeterminate(ctx context.Context, tr transfer.Transfer) error {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := j.balances.LockBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
		return err
	}
	current, err := lockTransferStatus(ctx, tx, tr.ID)
	if err != nil {
		return err
	}
	if current != transfer.StatusProcessing {
		return nil
	}
	if _, err := j.transfers.Transition(ctx, tx, tr.ID, transfer.StatusNeedsReview, "indeterminate payout"); err != nil {
		return err
	}
	if err := setLatestAttemptResult(ctx, tx, tr.ID, string(transfer.StatusNeedsReview)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockTransferStatus(ctx context.Context, tx pgx.Tx, transferID string) (transfer.Status, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM transfers WHERE transfer_id = $1 FOR UPDATE`, transferID).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("lock transfer: %w", err)
	}
	return transfer.Status(status), nil
}

func markAttemptResult(ctx context.Context, tx pgx.Tx, transferID, providerID, payoutID, result string) error {
	tag, err := tx.Exec(ctx, `
UPDATE provider_attempts SET result = $4
WHERE transfer_id = $1 AND provider_id = $2 AND payout_id = $3`,
		transferID, providerID, payoutID, result,
	)
	if err != nil {
		return fmt.Errorf("update provider attempt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("provider attempt not updated")
	}
	return nil
}

func setLatestAttemptResult(ctx context.Context, tx pgx.Tx, transferID, result string) error {
	_, err := tx.Exec(ctx, `
UPDATE provider_attempts SET result = $2
WHERE id = (
	SELECT id FROM provider_attempts WHERE transfer_id = $1 ORDER BY id DESC LIMIT 1
)`,
		transferID, result,
	)
	if err != nil {
		return fmt.Errorf("update latest provider attempt: %w", err)
	}
	return nil
}
