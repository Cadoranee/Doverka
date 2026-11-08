package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Cadoranee/Doverka/internal/ledger"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Continuer interface {
	Run(ctx context.Context, transferID, scenario string) error
}

type Worker struct {
	pool      *pgxpool.Pool
	transfers *transfer.Machine
	balances  *ledger.Ledger
	run       Continuer
}

func New(pool *pgxpool.Pool, transfers *transfer.Machine, balances *ledger.Ledger, run Continuer) *Worker {
	return &Worker{
		pool:      pool,
		transfers: transfers,
		balances:  balances,
		run:       run,
	}
}

func (w *Worker) Resume(ctx context.Context) error {
	if err := w.configured(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	ids, err := w.listInFlight(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := w.resumeOne(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("resume %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) listInFlight(ctx context.Context) ([]string, error) {
	rows, err := w.pool.Query(ctx, `
SELECT transfer_id FROM transfers
WHERE status IN ($1, $2, $3)
ORDER BY created_at, transfer_id`,
		string(transfer.StatusReserved),
		string(transfer.StatusSubmitting),
		string(transfer.StatusProcessing),
	)
	if err != nil {
		return nil, fmt.Errorf("list in-flight transfers: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list in-flight transfers: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list in-flight transfers: %w", err)
	}
	return ids, nil
}

func (w *Worker) configured() error {
	if w == nil || w.pool == nil || w.transfers == nil || w.balances == nil || w.run == nil {
		return errors.New("resume worker is not configured")
	}
	return nil
}

func (w *Worker) Run(ctx context.Context, transferID, scenario string) error {
	if err := w.configured(); err != nil {
		return err
	}
	err := w.run.Run(ctx, transferID, scenario)
	if errors.Is(err, transfer.ErrOutcomeUnknown) {
		bg := context.WithoutCancel(ctx)
		bg, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		return w.markNeedsReview(bg, transferID)
	}
	return err
}

func (w *Worker) resumeOne(ctx context.Context, id string) error {
	err := w.Run(ctx, id, "")
	if err != nil {
		return err
	}
	tr, err := w.transfers.Get(ctx, id)
	if err != nil {
		return err
	}
	if !tr.Status.Terminal() {
		return fmt.Errorf("non-terminal status %s", tr.Status)
	}
	return nil
}

func (w *Worker) markNeedsReview(ctx context.Context, transferID string) error {
	tr, err := w.transfers.Get(ctx, transferID)
	if err != nil {
		return err
	}
	if tr.Status.Terminal() {
		return nil
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := w.balances.LockBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
		return err
	}
	_, transErr := w.transfers.Transition(ctx, tx, transferID, transfer.StatusNeedsReview, "unresolved after restart")
	if transErr != nil {
		if errors.Is(transErr, transfer.ErrInvalidTransition) {
			_ = tx.Rollback(ctx)
			tr2, getErr := w.transfers.Get(ctx, transferID)
			if getErr != nil {
				return getErr
			}
			if tr2.Status.Terminal() {
				return nil
			}
			return transErr
		}
		return transErr
	}
	return tx.Commit(ctx)
}
