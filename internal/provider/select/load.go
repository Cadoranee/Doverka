package selection

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

func LoadProviders(path string) ([]Provider, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("providers path is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read providers: %w", err)
	}
	var rows []providerJSON
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("providers json: %w", err)
	}
	seen := make(map[string]struct{}, len(rows))
	out := make([]Provider, 0, len(rows))
	for i, row := range rows {
		p, err := providerFromJSON(row, i)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[p.ID]; dup {
			return nil, fmt.Errorf("providers[%d]: duplicate provider_id %q", i, p.ID)
		}
		seen[p.ID] = struct{}{}
		out = append(out, p)
	}
	return out, nil
}

type providerJSON struct {
	ProviderID         string       `json:"provider_id"`
	Directions         []string     `json:"directions"`
	Currency           string       `json:"currency"`
	Fee                feeJSON      `json:"fee"`
	Limits             limitsJSON   `json:"limits"`
	SuccessRate24h     string       `json:"success_rate_24h"`
	P95LatencyMs       int64        `json:"p95_latency_ms"`
	Enabled            bool         `json:"enabled"`
	MaintenanceWindows []windowJSON `json:"maintenance_windows"`
}

type feeJSON struct {
	Percent string `json:"percent"`
	Fixed   string `json:"fixed"`
}

type limitsJSON struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

type windowJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func providerFromJSON(row providerJSON, idx int) (Provider, error) {
	id := strings.TrimSpace(row.ProviderID)
	if id == "" {
		return Provider{}, fmt.Errorf("providers[%d]: empty provider_id", idx)
	}
	pct, err := parseProviderDecimal(row.Fee.Percent, idx, id, "fee.percent")
	if err != nil {
		return Provider{}, err
	}
	fixed, err := parseProviderDecimal(row.Fee.Fixed, idx, id, "fee.fixed")
	if err != nil {
		return Provider{}, err
	}
	limMin, err := parseProviderDecimal(row.Limits.Min, idx, id, "limits.min")
	if err != nil {
		return Provider{}, err
	}
	limMax, err := parseProviderDecimal(row.Limits.Max, idx, id, "limits.max")
	if err != nil {
		return Provider{}, err
	}
	success, err := parseProviderDecimal(row.SuccessRate24h, idx, id, "success_rate_24h")
	if err != nil {
		return Provider{}, err
	}
	windows, err := parseMaintenanceWindows(row.MaintenanceWindows, idx, id)
	if err != nil {
		return Provider{}, err
	}
	currency := strings.TrimSpace(row.Currency)
	if currency == "" {
		return Provider{}, fmt.Errorf("providers[%d] provider_id=%q: empty currency", idx, id)
	}
	dirs := make([]string, 0, len(row.Directions))
	for _, d := range row.Directions {
		d = strings.TrimSpace(d)
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	return Provider{
		ID:           id,
		Directions:   dirs,
		Currency:     currency,
		Fee:          Fee{Percent: pct, Fixed: fixed},
		Limits:       Limits{Min: limMin, Max: limMax},
		SuccessRate:  success,
		P95LatencyMs: row.P95LatencyMs,
		Enabled:      row.Enabled,
		Maintenance:  windows,
	}, nil
}

func parseProviderDecimal(raw string, idx int, id, field string) (decimal.Decimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return decimal.Decimal{}, fmt.Errorf("providers[%d] provider_id=%q: invalid %s", idx, id, field)
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("providers[%d] provider_id=%q: invalid %s: %w", idx, id, field, err)
	}
	return d, nil
}

func parseMaintenanceWindows(rows []windowJSON, idx int, id string) ([]Window, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]Window, 0, len(rows))
	for wi, row := range rows {
		from, err := time.Parse(time.RFC3339, strings.TrimSpace(row.From))
		if err != nil {
			return nil, fmt.Errorf("providers[%d] provider_id=%q: maintenance_windows[%d].from: %w", idx, id, wi, err)
		}
		to, err := time.Parse(time.RFC3339, strings.TrimSpace(row.To))
		if err != nil {
			return nil, fmt.Errorf("providers[%d] provider_id=%q: maintenance_windows[%d].to: %w", idx, id, wi, err)
		}
		out = append(out, Window{From: from.UTC(), To: to.UTC()})
	}
	return out, nil
}
