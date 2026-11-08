package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Cadoranee/Doverka/internal/calc"
	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

var (
	ErrDailyLimitExceeded = errors.New("DAILY_LIMIT_EXCEEDED")
	ErrDailyUsageNotFound = errors.New("daily usage not found")
)

type Converter interface {
	Convert(ctx context.Context, amount decimal.Decimal, from, to string) (decimal.Decimal, error)
}

type MinorScale interface {
	MinorUnits(currency string) (int, bool)
}

type DailyCharge struct {
	ClientID          string
	At                time.Time
	SourceCurrency    string
	SourceAmountMinor int64
	LimitAmount       decimal.Decimal
	LimitCurrency     string
}

type DailyBucket struct {
	Currency    string
	AmountMinor int64
}

func (l *Ledger) ReserveDaily(ctx context.Context, tx pgx.Tx, charge DailyCharge, scale MinorScale, convert Converter) error {
	if err := requireLedger(l); err != nil {
		return err
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := validateClientCurrency(charge.ClientID, charge.SourceCurrency); err != nil {
		return err
	}
	if err := validateClientCurrency(charge.ClientID, charge.LimitCurrency); err != nil {
		return err
	}
	if err := validateAmount(charge.SourceAmountMinor); err != nil {
		return err
	}
	if scale == nil {
		return errors.New("minor scale is nil")
	}
	if charge.SourceCurrency != charge.LimitCurrency && convert == nil {
		return errors.New("converter is nil")
	}
	limitUnits, err := currencyMinorUnits(scale, charge.LimitCurrency)
	if err != nil {
		return err
	}
	limitMinor, err := limitAmountMinor(charge.LimitAmount, limitUnits)
	if err != nil {
		return err
	}
	dateStr := utcUsageDate(charge.At)
	if err := advisoryLockClientDay(ctx, tx, charge.ClientID, dateStr); err != nil {
		return err
	}
	existing, err := loadDailyUsageForUpdate(ctx, tx, charge.ClientID, dateStr)
	if err != nil {
		return err
	}
	projected := make(map[string]int64, len(existing)+1)
	for c, a := range existing {
		projected[c] = a
	}
	prev := projected[charge.SourceCurrency]
	newAmt, overflow := addInt64(prev, charge.SourceAmountMinor)
	if overflow {
		return errors.New("amount overflow")
	}
	projected[charge.SourceCurrency] = newAmt
	totalLimitMinor, err := sumProjectedLimitMinor(ctx, projected, charge.LimitCurrency, scale, convert)
	if err != nil {
		return err
	}
	if totalLimitMinor > limitMinor {
		return ErrDailyLimitExceeded
	}
	_, err = tx.Exec(ctx, `
INSERT INTO daily_usage (client_id, usage_date, currency, amount_minor)
VALUES ($1, $2::date, $3, $4)
ON CONFLICT (client_id, usage_date, currency) DO UPDATE
SET amount_minor = EXCLUDED.amount_minor`,
		charge.ClientID, dateStr, charge.SourceCurrency, newAmt,
	)
	if err != nil {
		return fmt.Errorf("upsert daily usage: %w", err)
	}
	return nil
}

func (l *Ledger) ReleaseDaily(ctx context.Context, tx pgx.Tx, clientID string, at time.Time, currency string, amountMinor int64) error {
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
	dateStr := utcUsageDate(at)
	if err := advisoryLockClientDay(ctx, tx, clientID, dateStr); err != nil {
		return err
	}
	var stored int64
	err := tx.QueryRow(ctx, `
SELECT amount_minor FROM daily_usage
WHERE client_id = $1 AND usage_date = $2::date AND currency = $3
FOR UPDATE`,
		clientID, dateStr, currency,
	).Scan(&stored)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDailyUsageNotFound
		}
		return fmt.Errorf("lock daily usage: %w", err)
	}
	if stored < amountMinor {
		return errors.New("daily usage insufficient")
	}
	remainder := stored - amountMinor
	if remainder == 0 {
		_, err = tx.Exec(ctx, `
DELETE FROM daily_usage
WHERE client_id = $1 AND usage_date = $2::date AND currency = $3`,
			clientID, dateStr, currency,
		)
	} else {
		_, err = tx.Exec(ctx, `
UPDATE daily_usage SET amount_minor = $4
WHERE client_id = $1 AND usage_date = $2::date AND currency = $3`,
			clientID, dateStr, currency, remainder,
		)
	}
	if err != nil {
		return fmt.Errorf("update daily usage: %w", err)
	}
	return nil
}

func (l *Ledger) ReleaseDailyIfPresent(ctx context.Context, tx pgx.Tx, clientID string, at time.Time, currency string, amountMinor int64) error {
	err := l.ReleaseDaily(ctx, tx, clientID, at, currency, amountMinor)
	if err != nil && errors.Is(err, ErrDailyUsageNotFound) {
		return nil
	}
	return err
}

func (l *Ledger) DailyUsage(ctx context.Context, clientID string, at time.Time) ([]DailyBucket, error) {
	if err := requireLedger(l); err != nil {
		return nil, err
	}
	if clientID == "" {
		return nil, errors.New("client id is empty")
	}
	dateStr := utcUsageDate(at)
	rows, err := l.pool.Query(ctx, `
SELECT currency, amount_minor FROM daily_usage
WHERE client_id = $1 AND usage_date = $2::date
ORDER BY currency ASC`,
		clientID, dateStr,
	)
	if err != nil {
		return nil, fmt.Errorf("select daily usage: %w", err)
	}
	defer rows.Close()
	var out []DailyBucket
	for rows.Next() {
		var b DailyBucket
		if err := rows.Scan(&b.Currency, &b.AmountMinor); err != nil {
			return nil, fmt.Errorf("scan daily usage: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily usage: %w", err)
	}
	if out == nil {
		out = []DailyBucket{}
	}
	return out, nil
}

func utcUsageDate(at time.Time) string {
	return at.UTC().Format("2006-01-02")
}

func advisoryLockClientDay(ctx context.Context, tx pgx.Tx, clientID, dateStr string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, clientID, dateStr)
	if err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	return nil
}

func loadDailyUsageForUpdate(ctx context.Context, tx pgx.Tx, clientID, dateStr string) (map[string]int64, error) {
	rows, err := tx.Query(ctx, `
SELECT currency, amount_minor FROM daily_usage
WHERE client_id = $1 AND usage_date = $2::date
ORDER BY currency FOR UPDATE`,
		clientID, dateStr,
	)
	if err != nil {
		return nil, fmt.Errorf("select daily usage for update: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var currency string
		var amount int64
		if err := rows.Scan(&currency, &amount); err != nil {
			return nil, fmt.Errorf("scan daily usage row: %w", err)
		}
		out[currency] = amount
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily usage for update: %w", err)
	}
	return out, nil
}

func currencyMinorUnits(scale MinorScale, currency string) (int, error) {
	units, ok := scale.MinorUnits(currency)
	if !ok {
		return 0, fmt.Errorf("unknown currency %s", currency)
	}
	if units < 0 || units > 18 {
		return 0, fmt.Errorf("minor units out of range")
	}
	return units, nil
}

func limitAmountMinor(limit decimal.Decimal, limitUnits int) (int64, error) {
	if limit.IsNegative() {
		return 0, errors.New("limit amount must not be negative")
	}
	return wholeMinor(limit, limitUnits)
}

func wholeMinor(amount decimal.Decimal, units int) (int64, error) {
	scaled := amount.Shift(int32(units))
	if !scaled.Equal(scaled.Truncate(0)) {
		return 0, errors.New("amount is not whole minor units")
	}
	bi := scaled.BigInt()
	if bi.Sign() < 0 || !bi.IsInt64() {
		return 0, errors.New("amount overflow")
	}
	return bi.Int64(), nil
}

func sumProjectedLimitMinor(ctx context.Context, projected map[string]int64, limitCurrency string, scale MinorScale, convert Converter) (int64, error) {
	limitUnits, err := currencyMinorUnits(scale, limitCurrency)
	if err != nil {
		return 0, err
	}
	var total int64
	for currency, amountMinor := range projected {
		units, err := currencyMinorUnits(scale, currency)
		if err != nil {
			return 0, err
		}
		major := decimal.New(amountMinor, -int32(units))
		var converted decimal.Decimal
		if currency == limitCurrency {
			converted = major
		} else {
			converted, err = convert.Convert(ctx, major, currency, limitCurrency)
			if err != nil {
				return 0, fmt.Errorf("convert %s to %s: %w", currency, limitCurrency, err)
			}
		}
		rounded, err := calc.Round(converted, refdata.Currency{Code: limitCurrency, MinorUnits: limitUnits})
		if err != nil {
			return 0, err
		}
		part, err := wholeMinor(rounded.Amount, limitUnits)
		if err != nil {
			return 0, err
		}
		var overflow bool
		total, overflow = addInt64(total, part)
		if overflow {
			return 0, errors.New("amount overflow")
		}
	}
	return total, nil
}

func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, true
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, true
	}
	return a + b, false
}
