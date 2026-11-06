package refdata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shopspring/decimal"
)

type Currency struct {
	Code       string `json:"code"`
	MinorUnits int    `json:"minor_units"`
}

type Side struct {
	Country  string `json:"country"`
	Currency string `json:"currency"`
}

type Fee struct {
	Percent   decimal.Decimal
	Fixed     decimal.Decimal
	Min       decimal.Decimal
	ChargedIn string
}

type AmountLimits struct {
	Min      decimal.Decimal
	Max      decimal.Decimal
	Currency string
}

type Tariff struct {
	DirectionID      string
	Source           Side
	Target           Side
	PayoutMethod     string
	Fee              Fee
	AmountLimits     AmountLimits
	MinMarginPercent decimal.Decimal
	QuoteTTLSec      int
}

type Client struct {
	ClientID   string
	Balances   map[string]decimal.Decimal
	DailyLimit DailyLimit
}

type DailyLimit struct {
	Amount   decimal.Decimal
	Currency string
}

type Catalog struct {
	currencies         map[string]Currency
	tariffsByDirection map[string]Tariff
	tariffMatch        map[tariffKey]Tariff
	clients            map[string]Client
}

type tariffKey struct {
	sCountry, sCurrency, tCountry, tCurrency, payout string
}

func Load(dir string) (*Catalog, error) {
	root := strings.TrimSpace(dir)
	if root == "" {
		return nil, fmt.Errorf("refdata: data directory is empty")
	}

	curBytes, err := readDataFile(root, "currencies.json")
	if err != nil {
		return nil, err
	}
	tarBytes, err := readDataFile(root, "tariffs.json")
	if err != nil {
		return nil, err
	}
	clBytes, err := readDataFile(root, "clients.json")
	if err != nil {
		return nil, err
	}

	var curRows []Currency
	if err := json.Unmarshal(curBytes, &curRows); err != nil {
		return nil, fmt.Errorf("refdata: currencies.json: %w", err)
	}

	currencies := make(map[string]Currency, len(curRows))
	for i, row := range curRows {
		code := strings.TrimSpace(row.Code)
		if code == "" {
			return nil, fmt.Errorf("refdata: currencies.json[%d]: empty code", i)
		}
		if row.MinorUnits < 0 {
			return nil, fmt.Errorf("refdata: currencies.json[%d]: minor_units for %q must be >= 0", i, code)
		}
		if _, dup := currencies[code]; dup {
			return nil, fmt.Errorf("refdata: duplicate currency code %q", code)
		}
		currencies[code] = Currency{Code: code, MinorUnits: row.MinorUnits}
	}

	var tarRows []tariffJSON
	if err := json.Unmarshal(tarBytes, &tarRows); err != nil {
		return nil, fmt.Errorf("refdata: tariffs.json: %w", err)
	}

	tariffsByDirection := make(map[string]Tariff, len(tarRows))
	tariffMatch := make(map[tariffKey]Tariff, len(tarRows))
	for i, raw := range tarRows {
		tf, err := tariffFromJSON(raw, i)
		if err != nil {
			return nil, err
		}
		if _, dup := tariffsByDirection[tf.DirectionID]; dup {
			return nil, fmt.Errorf("refdata: duplicate direction_id %q", tf.DirectionID)
		}
		tariffsByDirection[tf.DirectionID] = tf
		key := tariffKey{
			sCountry:  tf.Source.Country,
			sCurrency: tf.Source.Currency,
			tCountry:  tf.Target.Country,
			tCurrency: tf.Target.Currency,
			payout:    tf.PayoutMethod,
		}
		if _, dup := tariffMatch[key]; dup {
			return nil, fmt.Errorf("refdata: duplicate tariff route %s/%s -> %s/%s %s", key.sCountry, key.sCurrency, key.tCountry, key.tCurrency, key.payout)
		}
		tariffMatch[key] = tf
	}

	var clRows []clientJSON
	if err := json.Unmarshal(clBytes, &clRows); err != nil {
		return nil, fmt.Errorf("refdata: clients.json: %w", err)
	}

	clients := make(map[string]Client, len(clRows))
	for i, raw := range clRows {
		c, err := clientFromJSON(raw, i)
		if err != nil {
			return nil, err
		}
		if _, dup := clients[c.ClientID]; dup {
			return nil, fmt.Errorf("refdata: duplicate client_id %q", c.ClientID)
		}
		clients[c.ClientID] = c
	}

	return &Catalog{
		currencies:         currencies,
		tariffsByDirection: tariffsByDirection,
		tariffMatch:        tariffMatch,
		clients:            clients,
	}, nil
}

func readDataFile(dir, name string) ([]byte, error) {
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("refdata: missing %s: %w", name, err)
		}
		return nil, fmt.Errorf("refdata: read %s: %w", name, err)
	}
	return b, nil
}

type tariffJSON struct {
	DirectionID  string `json:"direction_id"`
	Source       Side   `json:"source"`
	Target       Side   `json:"target"`
	PayoutMethod string `json:"payout_method"`
	Fee          struct {
		Percent   string `json:"percent"`
		Fixed     string `json:"fixed"`
		Min       string `json:"min"`
		ChargedIn string `json:"charged_in"`
	} `json:"fee"`
	AmountLimits struct {
		Min      string `json:"min"`
		Max      string `json:"max"`
		Currency string `json:"currency"`
	} `json:"amount_limits"`
	MinMarginPercent string `json:"min_margin_percent"`
	QuoteTTLSec      int    `json:"quote_ttl_sec"`
}

func tariffFromJSON(raw tariffJSON, i int) (Tariff, error) {
	prefix := fmt.Sprintf("refdata: tariffs.json[%d]", i)
	dirID := strings.TrimSpace(raw.DirectionID)
	if dirID == "" {
		return Tariff{}, fmt.Errorf("%s: empty direction_id", prefix)
	}
	sc := strings.TrimSpace(raw.Source.Country)
	sy := strings.TrimSpace(raw.Source.Currency)
	tc := strings.TrimSpace(raw.Target.Country)
	ty := strings.TrimSpace(raw.Target.Currency)
	if sc == "" || sy == "" || tc == "" || ty == "" {
		return Tariff{}, fmt.Errorf("%s direction %q: empty country or currency in side", prefix, dirID)
	}
	pm := strings.TrimSpace(raw.PayoutMethod)
	if pm == "" {
		return Tariff{}, fmt.Errorf("%s direction %q: empty payout_method", prefix, dirID)
	}
	ci := strings.TrimSpace(raw.Fee.ChargedIn)
	switch ci {
	case "SOURCE", "TARGET":
	default:
		return Tariff{}, fmt.Errorf("%s direction %q: invalid charged_in %q", prefix, dirID, raw.Fee.ChargedIn)
	}

	pct, err := parseDecimal(raw.Fee.Percent, fmt.Sprintf("%s direction %q: fee.percent", prefix, dirID))
	if err != nil {
		return Tariff{}, err
	}
	fix, err := parseDecimal(raw.Fee.Fixed, fmt.Sprintf("%s direction %q: fee.fixed", prefix, dirID))
	if err != nil {
		return Tariff{}, err
	}
	fmin, err := parseDecimal(raw.Fee.Min, fmt.Sprintf("%s direction %q: fee.min", prefix, dirID))
	if err != nil {
		return Tariff{}, err
	}
	amin, err := parseDecimal(raw.AmountLimits.Min, fmt.Sprintf("%s direction %q: amount_limits.min", prefix, dirID))
	if err != nil {
		return Tariff{}, err
	}
	amax, err := parseDecimal(raw.AmountLimits.Max, fmt.Sprintf("%s direction %q: amount_limits.max", prefix, dirID))
	if err != nil {
		return Tariff{}, err
	}
	alCur := strings.TrimSpace(raw.AmountLimits.Currency)
	if alCur == "" {
		return Tariff{}, fmt.Errorf("%s direction %q: empty amount_limits.currency", prefix, dirID)
	}
	mmp, err := parseDecimal(raw.MinMarginPercent, fmt.Sprintf("%s direction %q: min_margin_percent", prefix, dirID))
	if err != nil {
		return Tariff{}, err
	}
	if raw.QuoteTTLSec < 0 {
		return Tariff{}, fmt.Errorf("%s direction %q: quote_ttl_sec must be >= 0", prefix, dirID)
	}

	return Tariff{
		DirectionID:  dirID,
		Source:       Side{Country: sc, Currency: sy},
		Target:       Side{Country: tc, Currency: ty},
		PayoutMethod: pm,
		Fee: Fee{
			Percent:   pct,
			Fixed:     fix,
			Min:       fmin,
			ChargedIn: ci,
		},
		AmountLimits: AmountLimits{
			Min:      amin,
			Max:      amax,
			Currency: alCur,
		},
		MinMarginPercent: mmp,
		QuoteTTLSec:      raw.QuoteTTLSec,
	}, nil
}

type clientJSON struct {
	ClientID   string            `json:"client_id"`
	Balances   map[string]string `json:"balances"`
	DailyLimit struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"daily_limit"`
}

func clientFromJSON(raw clientJSON, i int) (Client, error) {
	prefix := fmt.Sprintf("refdata: clients.json[%d]", i)
	id := strings.TrimSpace(raw.ClientID)
	if id == "" {
		return Client{}, fmt.Errorf("%s: empty client_id", prefix)
	}
	if raw.Balances == nil {
		return Client{}, fmt.Errorf("%s client %q: missing balances", prefix, id)
	}
	bal := make(map[string]decimal.Decimal, len(raw.Balances))
	for k, v := range raw.Balances {
		kk := strings.TrimSpace(k)
		if kk == "" {
			return Client{}, fmt.Errorf("%s client %q: empty balance currency key", prefix, id)
		}
		if _, dup := bal[kk]; dup {
			return Client{}, fmt.Errorf("%s client %q: duplicate balance currency %q", prefix, id, kk)
		}
		d, err := decimal.NewFromString(v)
		if err != nil {
			return Client{}, fmt.Errorf("%s client %q balance %q: %w", prefix, id, kk, err)
		}
		bal[kk] = d
	}
	dlAmt, err := parseDecimal(raw.DailyLimit.Amount, fmt.Sprintf("%s client %q: daily_limit.amount", prefix, id))
	if err != nil {
		return Client{}, err
	}
	dlCur := strings.TrimSpace(raw.DailyLimit.Currency)
	if dlCur == "" {
		return Client{}, fmt.Errorf("%s client %q: empty daily_limit.currency", prefix, id)
	}
	return Client{
		ClientID: id,
		Balances: bal,
		DailyLimit: DailyLimit{
			Amount:   dlAmt,
			Currency: dlCur,
		},
	}, nil
}

func parseDecimal(s, ctx string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("%s: %w", ctx, err)
	}
	return d, nil
}

func (c *Catalog) Currency(code string) (Currency, bool) {
	if c == nil {
		return Currency{}, false
	}
	v, ok := c.currencies[code]
	return v, ok
}

func (c *Catalog) Tariff(directionID string) (Tariff, bool) {
	if c == nil {
		return Tariff{}, false
	}
	t, ok := c.tariffsByDirection[directionID]
	if !ok {
		return Tariff{}, false
	}
	return copyTariff(t), true
}

func (c *Catalog) FindTariff(sourceCountry, sourceCurrency, targetCountry, targetCurrency, payoutMethod string) (Tariff, bool) {
	if c == nil {
		return Tariff{}, false
	}
	key := tariffKey{
		sCountry:  sourceCountry,
		sCurrency: sourceCurrency,
		tCountry:  targetCountry,
		tCurrency: targetCurrency,
		payout:    payoutMethod,
	}
	t, ok := c.tariffMatch[key]
	if !ok {
		return Tariff{}, false
	}
	return copyTariff(t), true
}

func copyTariff(t Tariff) Tariff {
	return Tariff{
		DirectionID:  t.DirectionID,
		Source:       t.Source,
		Target:       t.Target,
		PayoutMethod: t.PayoutMethod,
		Fee: Fee{
			Percent:   t.Fee.Percent.Copy(),
			Fixed:     t.Fee.Fixed.Copy(),
			Min:       t.Fee.Min.Copy(),
			ChargedIn: t.Fee.ChargedIn,
		},
		AmountLimits: AmountLimits{
			Min:      t.AmountLimits.Min.Copy(),
			Max:      t.AmountLimits.Max.Copy(),
			Currency: t.AmountLimits.Currency,
		},
		MinMarginPercent: t.MinMarginPercent.Copy(),
		QuoteTTLSec:      t.QuoteTTLSec,
	}
}

func (c *Catalog) Client(id string) (Client, bool) {
	if c == nil {
		return Client{}, false
	}
	cl, ok := c.clients[id]
	if !ok {
		return Client{}, false
	}
	out := Client{
		ClientID: cl.ClientID,
		Balances: make(map[string]decimal.Decimal, len(cl.Balances)),
		DailyLimit: DailyLimit{
			Amount:   cl.DailyLimit.Amount.Copy(),
			Currency: cl.DailyLimit.Currency,
		},
	}
	for k, v := range cl.Balances {
		out.Balances[k] = v.Copy()
	}
	return out, true
}
