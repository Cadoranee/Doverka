package quote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

var ErrNotFound = errors.New("quote not found")

type Snapshot struct {
	DirectionID          string
	SourceCountry        string
	SourceCurrency       string
	TargetCountry        string
	TargetCurrency       string
	PayoutMethod         string
	AmountType           string
	SourceAmountMinor    int64
	FeeAmountMinor       int64
	FeeCurrency          string
	AmountToConvertMinor int64
	Rate                 decimal.Decimal
	TargetAmountMinor    int64
	Breakdown            json.RawMessage
}

type Quote struct {
	ID string
	Snapshot
	ExpiresAt time.Time
	CreatedAt time.Time
}

func (q Quote) Expired(now time.Time) bool {
	return !now.UTC().Before(q.ExpiresAt.UTC())
}

type Repo struct {
	pool *pgxpool.Pool
	clk  clock.Clock
}

func NewRepo(pool *pgxpool.Pool, clk clock.Clock) *Repo {
	return &Repo{pool: pool, clk: clk}
}

func (r *Repo) Save(ctx context.Context, snap Snapshot, ttl time.Duration) (Quote, error) {
	if r == nil || r.pool == nil || r.clk == nil {
		return Quote{}, errors.New("quote repository is not configured")
	}
	id, err := newQuoteID()
	if err != nil {
		return Quote{}, err
	}
	now := r.clk.Now().UTC()
	expiresAt := now.Add(ttl)
	breakdown := breakdownBytes(snap.Breakdown)
	_, err = r.pool.Exec(ctx, `
INSERT INTO quotes (
	quote_id, direction_id, source_country, source_currency, target_country, target_currency,
	payout_method, amount_type, source_amount_minor, fee_amount_minor, fee_currency,
	amount_to_convert_minor, rate, target_amount_minor, breakdown, expires_at, created_at
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::numeric, $14, $15::jsonb, $16, $17
)`,
		id,
		snap.DirectionID,
		snap.SourceCountry,
		snap.SourceCurrency,
		snap.TargetCountry,
		snap.TargetCurrency,
		snap.PayoutMethod,
		snap.AmountType,
		snap.SourceAmountMinor,
		snap.FeeAmountMinor,
		snap.FeeCurrency,
		snap.AmountToConvertMinor,
		snap.Rate.String(),
		snap.TargetAmountMinor,
		string(breakdown),
		expiresAt,
		now,
	)
	if err != nil {
		return Quote{}, fmt.Errorf("insert quote: %w", err)
	}
	return Quote{
		ID: id,
		Snapshot: Snapshot{
			DirectionID:          snap.DirectionID,
			SourceCountry:        snap.SourceCountry,
			SourceCurrency:       snap.SourceCurrency,
			TargetCountry:        snap.TargetCountry,
			TargetCurrency:       snap.TargetCurrency,
			PayoutMethod:         snap.PayoutMethod,
			AmountType:           snap.AmountType,
			SourceAmountMinor:    snap.SourceAmountMinor,
			FeeAmountMinor:       snap.FeeAmountMinor,
			FeeCurrency:          snap.FeeCurrency,
			AmountToConvertMinor: snap.AmountToConvertMinor,
			Rate:                 snap.Rate,
			TargetAmountMinor:    snap.TargetAmountMinor,
			Breakdown:            append(json.RawMessage(nil), breakdown...),
		},
		ExpiresAt: expiresAt,
		CreatedAt: now,
	}, nil
}

func (r *Repo) Get(ctx context.Context, quoteID string) (Quote, error) {
	if r == nil || r.pool == nil {
		return Quote{}, errors.New("quote repository is not configured")
	}
	if quoteID == "" {
		return Quote{}, ErrNotFound
	}
	var q Quote
	var rateText string
	var breakdown []byte
	err := r.pool.QueryRow(ctx, `
SELECT
	quote_id, direction_id, source_country, source_currency, target_country, target_currency,
	payout_method, amount_type, source_amount_minor, fee_amount_minor, fee_currency,
	amount_to_convert_minor, rate::text, target_amount_minor, breakdown, expires_at, created_at
FROM quotes
WHERE quote_id = $1`,
		quoteID,
	).Scan(
		&q.ID,
		&q.DirectionID,
		&q.SourceCountry,
		&q.SourceCurrency,
		&q.TargetCountry,
		&q.TargetCurrency,
		&q.PayoutMethod,
		&q.AmountType,
		&q.SourceAmountMinor,
		&q.FeeAmountMinor,
		&q.FeeCurrency,
		&q.AmountToConvertMinor,
		&rateText,
		&q.TargetAmountMinor,
		&breakdown,
		&q.ExpiresAt,
		&q.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quote{}, ErrNotFound
		}
		return Quote{}, fmt.Errorf("select quote: %w", err)
	}
	q.Rate, err = decimal.NewFromString(strings.TrimSpace(rateText))
	if err != nil {
		return Quote{}, fmt.Errorf("parse rate: %w", err)
	}
	q.Breakdown = json.RawMessage(breakdown)
	return q, nil
}

func newQuoteID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:]), nil
}

func breakdownBytes(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("[]")
	}
	return []byte(raw)
}
