package transfer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/ledger"
	providerclient "github.com/Cadoranee/Doverka/internal/provider/client"
	selection "github.com/Cadoranee/Doverka/internal/provider/select"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

var ErrOutcomeUnknown = errors.New("provider outcome unknown")

var errAlreadySettled = errors.New("transfer already settled")

const (
	attemptResultUnknown = "unknown"
	payoutPollInterval   = 500 * time.Millisecond
	visibilityMargin     = 5 * time.Second
)

type ExecuteInput struct {
	TransferID string
	Scenario   string
	MinorUnits int
	Ranked     selection.Ranked
}

type ProviderAttempt struct {
	ProviderID string
	PayoutID   string
	Result     string
	At         time.Time
}

type Executor struct {
	pool         *pgxpool.Pool
	transfers    *Machine
	balances     *ledger.Ledger
	payouts      *providerclient.Client
	clk          clock.Clock
	unknownPoll  providerclient.UnknownPoll
	pollInterval time.Duration
}

func NewExecutor(pool *pgxpool.Pool, transfers *Machine, balances *ledger.Ledger, payouts *providerclient.Client, clk clock.Clock) *Executor {
	return &Executor{
		pool:      pool,
		transfers: transfers,
		balances:  balances,
		payouts:   payouts,
		clk:       clk,
		unknownPoll: providerclient.UnknownPoll{
			Window:   providerclient.VisibilityWindow + visibilityMargin,
			Interval: providerclient.PollInterval,
			Clock:    clk,
		},
		pollInterval: payoutPollInterval,
	}
}

func (e *Executor) Execute(ctx context.Context, in ExecuteInput) error {
	if err := requireExecutor(e); err != nil {
		return err
	}
	if in.TransferID == "" {
		return errors.New("transfer id is empty")
	}
	if in.MinorUnits < 0 {
		return errors.New("minor units must be non-negative")
	}

	tr, err := e.transfers.Get(ctx, in.TransferID)
	if err != nil {
		return err
	}
	if tr.Status.Terminal() {
		return nil
	}
	if tr.Status == StatusProcessing {
		return e.resumeProcessing(ctx, in, tr)
	}
	if tr.Status != StatusReserved && tr.Status != StatusSubmitting {
		return fmt.Errorf("transfer status %s cannot be executed", tr.Status)
	}

	for {
		attempts, err := e.loadAttempts(ctx, in.TransferID)
		if err != nil {
			return err
		}
		if len(attempts) > 0 && attempts[len(attempts)-1].Result == attemptResultUnknown {
			return ErrOutcomeUnknown
		}
		selAttempts := make([]selection.Attempt, len(attempts))
		for i, a := range attempts {
			selAttempts[i] = selection.Attempt{ProviderID: a.ProviderID, Code: a.Result}
		}
		candidate, ok := in.Ranked.Next(selAttempts)
		if !ok {
			return e.failExhausted(ctx, tr)
		}

		if err := e.transitionSubmitting(ctx, tr, candidate.Provider.ID); err != nil {
			return err
		}

		attemptNum := len(attempts) + 1
		amount := formatProviderAmount(tr.TargetAmountMinor, in.MinorUnits)
		createRes, createErr := e.payouts.CreatePayout(ctx, providerclient.CreateRequest{
			ProviderID: candidate.Provider.ID,
			TransferID: in.TransferID,
			Attempt:    attemptNum,
			Amount:     amount,
			Currency:   tr.TargetCurrency,
			Recipient:  tr.Recipient,
			Scenario:   in.Scenario,
		})
		switch {
		case createErr == nil && createRes.Outcome == providerclient.OutcomeCreated:
			return e.handleCreated(ctx, in, tr, candidate.Provider.ID, attemptNum, createRes)
		case createErr == nil && (createRes.Outcome == providerclient.OutcomeDeclined || createRes.Outcome == providerclient.OutcomeProviderError):
			if err := e.insertDeclinedAttempt(ctx, tr, candidate.Provider.ID, attemptNum, declineCode(createRes)); err != nil {
				return err
			}
			continue
		}
		retry, err := e.resolveUnknown(ctx, in, tr, candidate.Provider.ID, attemptNum, unknownCause(createRes, createErr))
		if retry {
			continue
		}
		return err
	}
}

func declineCode(res providerclient.CreateResult) string {
	switch {
	case res.Code != "":
		return res.Code
	case res.Outcome == providerclient.OutcomeProviderError:
		return selection.InternalError
	default:
		return res.Outcome
	}
}

func unknownCause(res providerclient.CreateResult, err error) error {
	switch {
	case err != nil:
		return err
	case res.Outcome != providerclient.OutcomeUnknown:
		return fmt.Errorf("unexpected outcome %q", res.Outcome)
	case res.Detail != "":
		return errors.New(res.Detail)
	default:
		return errors.New(providerclient.OutcomeUnknown)
	}
}

func (e *Executor) Attempts(ctx context.Context, transferID string) ([]ProviderAttempt, error) {
	if err := requireExecutor(e); err != nil {
		return nil, err
	}
	return e.loadAttempts(ctx, transferID)
}

func (e *Executor) resumeProcessing(ctx context.Context, in ExecuteInput, tr Transfer) error {
	attempts, err := e.loadAttempts(ctx, in.TransferID)
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		return fmt.Errorf("processing transfer has no provider attempts")
	}
	last := attempts[len(attempts)-1]
	if last.Result == attemptResultUnknown {
		return ErrOutcomeUnknown
	}
	if last.PayoutID == "" {
		return fmt.Errorf("processing transfer has no payout id on last attempt")
	}
	return e.settlePayout(ctx, tr, last.ProviderID, last.PayoutID, attemptKey(in.TransferID, len(attempts)))
}

func (e *Executor) handleCreated(ctx context.Context, in ExecuteInput, tr Transfer, providerID string, attemptNum int, createRes providerclient.CreateResult) error {
	idempotencyKey := attemptKey(in.TransferID, attemptNum)
	if err := e.runInTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockClientBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
			return err
		}
		now := e.clk.Now().UTC()
		if err := insertProviderAttempt(ctx, tx, in.TransferID, providerID, idempotencyKey, createRes.PayoutID, "PROCESSING", now); err != nil {
			return err
		}
		_, err := e.transfers.Transition(ctx, tx, in.TransferID, StatusProcessing, createRes.PayoutID)
		return err
	}); err != nil {
		return err
	}

	switch createRes.Status {
	case "COMPLETED":
		return e.settleSuccess(ctx, tr, idempotencyKey)
	case "FAILED":
		return e.settleFailure(ctx, tr, idempotencyKey, "FAILED")
	default:
		return e.settlePayout(ctx, tr, providerID, createRes.PayoutID, idempotencyKey)
	}
}

func (e *Executor) resolveUnknown(ctx context.Context, in ExecuteInput, tr Transfer, providerID string, attemptNum int, cause error) (retry bool, err error) {
	observedAt := e.clk.Now().UTC()
	res, payout, pollErr := e.payouts.ResolveUnknown(ctx, providerID, tr.ID, observedAt, e.unknownPoll)
	switch res {
	case providerclient.ResolutionPresent:
		return false, e.handleCreated(ctx, in, tr, providerID, attemptNum, providerclient.CreateResult{
			Outcome:  providerclient.OutcomeCreated,
			PayoutID: payout.PayoutID,
			Status:   payout.Status,
		})
	case providerclient.ResolutionAbsent:
		if err := e.insertDeclinedAttempt(ctx, tr, providerID, attemptNum, selection.PayoutAbsent); err != nil {
			return false, err
		}
		return true, nil
	default:
		recordCause := cause
		if pollErr != nil {
			recordCause = pollErr
		}
		return false, e.recordUnknown(ctx, tr, providerID, attemptNum, recordCause)
	}
}

func (e *Executor) settlePayout(ctx context.Context, tr Transfer, providerID, payoutID, idempotencyKey string) error {
	status, failureCode, err := e.waitPayout(ctx, tr.ID, providerID, payoutID)
	if err != nil {
		if errors.Is(err, errAlreadySettled) {
			return nil
		}
		return err
	}
	switch status {
	case "COMPLETED":
		return e.settleSuccess(ctx, tr, idempotencyKey)
	case "FAILED":
		reason := failureCode
		if reason == "" {
			reason = "FAILED"
		}
		return e.settleFailure(ctx, tr, idempotencyKey, reason)
	default:
		return fmt.Errorf("unexpected payout status %q", status)
	}
}

func (e *Executor) waitPayout(ctx context.Context, transferID, providerID, payoutID string) (status, failureCode string, err error) {
	interval := e.pollInterval
	if interval <= 0 {
		interval = payoutPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		tr, getTrErr := e.transfers.Get(ctx, transferID)
		if getTrErr != nil {
			return "", "", getTrErr
		}
		if tr.Status.Terminal() {
			return "", "", errAlreadySettled
		}
		payout, found, getErr := e.payouts.GetPayout(ctx, providerID, payoutID)
		if getErr != nil {
			return "", "", getErr
		}
		if found {
			switch payout.Status {
			case "COMPLETED", "FAILED":
				return payout.Status, payout.FailureCode, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *Executor) settleSuccess(ctx context.Context, tr Transfer, idempotencyKey string) error {
	return e.runInTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockClientBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
			return err
		}
		st, err := lockTransferStatus(ctx, tx, tr.ID)
		if err != nil {
			return err
		}
		if st.Terminal() {
			return nil
		}
		if err := e.balances.Debit(ctx, tx, tr.ClientID, tr.SourceCurrency, tr.ID, tr.SourceAmountMinor); err != nil {
			return err
		}
		if _, err := e.transfers.Transition(ctx, tx, tr.ID, StatusCompleted, "completed"); err != nil {
			return err
		}
		return updateAttemptResult(ctx, tx, idempotencyKey, "COMPLETED")
	})
}

func (e *Executor) settleFailure(ctx context.Context, tr Transfer, idempotencyKey, reason string) error {
	return e.runInTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockClientBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
			return err
		}
		st, err := lockTransferStatus(ctx, tx, tr.ID)
		if err != nil {
			return err
		}
		if st.Terminal() {
			return nil
		}
		if err := e.balances.Release(ctx, tx, tr.ClientID, tr.SourceCurrency, tr.ID, tr.SourceAmountMinor); err != nil {
			return err
		}
		if err := e.balances.ReleaseDailyIfPresent(ctx, tx, tr.ClientID, tr.CreatedAt, tr.SourceCurrency, tr.SourceAmountMinor); err != nil {
			return err
		}
		if _, err := e.transfers.Transition(ctx, tx, tr.ID, StatusFailed, reason); err != nil {
			return err
		}
		return updateAttemptResult(ctx, tx, idempotencyKey, reason)
	})
}

func (e *Executor) transitionSubmitting(ctx context.Context, tr Transfer, providerID string) error {
	return e.runInTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockClientBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
			return err
		}
		_, err := e.transfers.Transition(ctx, tx, tr.ID, StatusSubmitting, providerID)
		return err
	})
}

func (e *Executor) insertDeclinedAttempt(ctx context.Context, tr Transfer, providerID string, attemptNum int, result string) error {
	return e.runInTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockClientBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
			return err
		}
		return insertProviderAttempt(ctx, tx, tr.ID, providerID, attemptKey(tr.ID, attemptNum), "", result, e.clk.Now().UTC())
	})
}

func (e *Executor) recordUnknown(ctx context.Context, tr Transfer, providerID string, attemptNum int, cause error) error {
	if providerID != "" {
		err := e.runInTx(ctx, func(tx pgx.Tx) error {
			if _, _, err := lockClientBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
				return err
			}
			return insertProviderAttempt(ctx, tx, tr.ID, providerID, attemptKey(tr.ID, attemptNum), "", attemptResultUnknown, e.clk.Now().UTC())
		})
		if err != nil {
			return err
		}
	}
	if cause == nil {
		return ErrOutcomeUnknown
	}
	return fmt.Errorf("%w: %v", ErrOutcomeUnknown, cause)
}

func (e *Executor) failExhausted(ctx context.Context, tr Transfer) error {
	return e.runInTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockClientBalance(ctx, tx, tr.ClientID, tr.SourceCurrency); err != nil {
			return err
		}
		if err := e.balances.Release(ctx, tx, tr.ClientID, tr.SourceCurrency, tr.ID, tr.SourceAmountMinor); err != nil {
			return err
		}
		if err := e.balances.ReleaseDailyIfPresent(ctx, tx, tr.ClientID, tr.CreatedAt, tr.SourceCurrency, tr.SourceAmountMinor); err != nil {
			return err
		}
		status, err := lockTransferStatus(ctx, tx, tr.ID)
		if err != nil {
			return err
		}
		switch status {
		case StatusReserved:
			if _, err := e.transfers.Transition(ctx, tx, tr.ID, StatusSubmitting, "no provider"); err != nil {
				return err
			}
		case StatusSubmitting:
		default:
			return fmt.Errorf("cannot fail transfer in status %s", status)
		}
		_, err = e.transfers.Transition(ctx, tx, tr.ID, StatusFailed, "providers exhausted")
		return err
	})
}

func (e *Executor) loadAttempts(ctx context.Context, transferID string) ([]ProviderAttempt, error) {
	return e.transfers.Attempts(ctx, transferID)
}

func (e *Executor) runInTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func requireExecutor(e *Executor) error {
	if e == nil || e.pool == nil || e.transfers == nil || e.balances == nil || e.payouts == nil || e.clk == nil {
		return errors.New("executor is not configured")
	}
	return nil
}

func formatProviderAmount(targetMinor int64, minorUnits int) string {
	return decimal.New(targetMinor, -int32(minorUnits)).StringFixed(int32(minorUnits))
}

func attemptKey(transferID string, attempt int) string {
	return fmt.Sprintf("%s:%d", transferID, attempt)
}

func lockTransferStatus(ctx context.Context, tx pgx.Tx, transferID string) (Status, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM transfers WHERE transfer_id = $1 FOR UPDATE`, transferID).Scan(&status); err != nil {
		return "", fmt.Errorf("read transfer status: %w", err)
	}
	return Status(status), nil
}

func lockClientBalance(ctx context.Context, tx pgx.Tx, clientID, currency string) (int64, int64, error) {
	var available, reserved int64
	err := tx.QueryRow(ctx, `
SELECT available, reserved FROM client_balances
WHERE client_id = $1 AND currency = $2
FOR UPDATE`, clientID, currency).Scan(&available, &reserved)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, ledger.ErrNotFound
		}
		return 0, 0, fmt.Errorf("lock balance: %w", err)
	}
	return available, reserved, nil
}

func insertProviderAttempt(ctx context.Context, tx pgx.Tx, transferID, providerID, idempotencyKey, payoutID, result string, at time.Time) error {
	var payout any
	if payoutID != "" {
		payout = payoutID
	}
	var res any
	if result != "" {
		res = result
	}
	_, err := tx.Exec(ctx, `
INSERT INTO provider_attempts (transfer_id, provider_id, idempotency_key, payout_id, result, attempted_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		transferID, providerID, idempotencyKey, payout, res, at)
	if err != nil {
		return fmt.Errorf("insert provider attempt: %w", err)
	}
	return nil
}

func updateAttemptResult(ctx context.Context, tx pgx.Tx, idempotencyKey, result string) error {
	tag, err := tx.Exec(ctx, `UPDATE provider_attempts SET result = $2 WHERE idempotency_key = $1`, idempotencyKey, result)
	if err != nil {
		return fmt.Errorf("update provider attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("provider attempt %s not found", idempotencyKey)
	}
	return nil
}
