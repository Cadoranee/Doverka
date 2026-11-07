package rates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/shopspring/decimal"
)

const StaleAfter = 6 * time.Hour

var ErrUnavailable = errors.New("RATE_UNAVAILABLE")

type Rate struct {
	Base      string
	Quote     string
	Value     decimal.Decimal
	UpdatedAt time.Time
}

type Source interface {
	Fetch(ctx context.Context) ([]Rate, error)
}

type Service struct {
	src Source
	clk clock.Clock
}

func New(src Source, clk clock.Clock) *Service {
	if clk == nil {
		clk = clock.System{}
	}
	return &Service{src: src, clk: clk}
}

func (s *Service) Rate(ctx context.Context, base, quote string) (decimal.Decimal, error) {
	base = strings.TrimSpace(base)
	quote = strings.TrimSpace(quote)
	if base == "" || quote == "" {
		return decimal.Decimal{}, ErrUnavailable
	}
	if base == quote {
		return decimal.NewFromInt(1), nil
	}
	snapshot, err := s.src.Fetch(ctx)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("%w: %s", ErrUnavailable, err.Error())
	}
	return resolve(snapshot, s.clk.Now(), base, quote)
}

func (s *Service) Convert(ctx context.Context, amount decimal.Decimal, base, quote string) (decimal.Decimal, error) {
	base = strings.TrimSpace(base)
	quote = strings.TrimSpace(quote)
	if base == "" || quote == "" {
		return decimal.Decimal{}, ErrUnavailable
	}
	if base == quote {
		return amount, nil
	}
	rate, err := s.Rate(ctx, base, quote)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return amount.Mul(rate), nil
}

type HTTPSource struct {
	baseURL string
	client  *http.Client
}

func NewHTTPSource(baseURL string, client *http.Client) *HTTPSource {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPSource{baseURL: baseURL, client: client}
}

func (h *HTTPSource) Fetch(ctx context.Context) ([]Rate, error) {
	url := strings.TrimRight(h.baseURL, "/") + "/rates"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseRatesJSON(data)
}

type rateJSON struct {
	Base      string `json:"base"`
	Quote     string `json:"quote"`
	Rate      string `json:"rate"`
	UpdatedAt string `json:"updated_at"`
}

func parseRatesJSON(data []byte) ([]Rate, error) {
	var rows []rateJSON
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	out := make([]Rate, 0, len(rows))
	for _, row := range rows {
		base := strings.TrimSpace(row.Base)
		quote := strings.TrimSpace(row.Quote)
		if base == "" || quote == "" {
			return nil, fmt.Errorf("empty currency code")
		}
		value, err := decimal.NewFromString(row.Rate)
		if err != nil {
			return nil, err
		}
		updatedAt, err := time.Parse(time.RFC3339, row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, Rate{
			Base:      base,
			Quote:     quote,
			Value:     value,
			UpdatedAt: updatedAt,
		})
	}
	return out, nil
}

type rateKey struct {
	base, quote string
}

func resolve(snapshot []Rate, now time.Time, base, quote string) (decimal.Decimal, error) {
	order, byPair := indexSnapshot(snapshot)
	if row, ok := byPair[rateKey{base, quote}]; ok {
		if usable(row, now) {
			return row.Value, nil
		}
		return decimal.Decimal{}, ErrUnavailable
	}
	if row, ok := byPair[rateKey{quote, base}]; ok {
		if usable(row, now) {
			return decimal.NewFromInt(1).DivRound(row.Value, 28), nil
		}
		return decimal.Decimal{}, ErrUnavailable
	}
	for _, key := range order {
		if key.base != base {
			continue
		}
		leg := byPair[key]
		if !usable(leg, now) {
			continue
		}
		onward, ok := byPair[rateKey{key.quote, quote}]
		if !ok || !usable(onward, now) {
			continue
		}
		return leg.Value.Mul(onward.Value), nil
	}
	return decimal.Decimal{}, ErrUnavailable
}

func indexSnapshot(snapshot []Rate) ([]rateKey, map[rateKey]Rate) {
	order := make([]rateKey, 0, len(snapshot))
	byPair := make(map[rateKey]Rate, len(snapshot))
	for _, row := range snapshot {
		key := rateKey{row.Base, row.Quote}
		if _, seen := byPair[key]; !seen {
			order = append(order, key)
		}
		byPair[key] = row
	}
	return order, byPair
}

func isFresh(updatedAt, now time.Time) bool {
	if updatedAt.IsZero() {
		return false
	}
	return now.Sub(updatedAt) <= StaleAfter
}

func usable(row Rate, now time.Time) bool {
	return isFresh(row.UpdatedAt, now) && row.Value.IsPositive()
}
