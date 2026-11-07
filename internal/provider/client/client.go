package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	OutcomeCreated       = "created"
	OutcomeDeclined      = "declined"
	OutcomeProviderError = "provider_error"
	OutcomeUnknown       = "unknown"

	connectTimeout  = 10 * time.Second
	readTimeout     = 35 * time.Second
	maxProviderBody = 1 << 20
)

type CreateRequest struct {
	ProviderID string
	TransferID string
	Attempt    int
	Amount     string
	Currency   string
	Recipient  json.RawMessage
	Scenario   string
}

type CreateResult struct {
	Outcome  string
	PayoutID string
	Status   string
	Code     string
	Detail   string
}

type Payout struct {
	PayoutID    string
	Reference   string
	Status      string
	FailureCode string
}

type Client struct {
	baseURL    string
	publicURL  string
	httpClient *http.Client
}

func New(baseURL, publicURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		publicURL:  strings.TrimRight(strings.TrimSpace(publicURL), "/"),
		httpClient: httpClient,
	}
}

func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   connectTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.ResponseHeaderTimeout = readTimeout
	return &http.Client{
		Timeout:   connectTimeout + readTimeout,
		Transport: transport,
	}
}

func (c *Client) CreatePayout(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateCreate(c.baseURL, c.publicURL, req); err != nil {
		return CreateResult{}, err
	}
	body, err := encodeCreateBody(c.publicURL, req)
	if err != nil {
		return CreateResult{}, err
	}
	endpoint := c.baseURL + "/" + url.PathEscape(req.ProviderID) + "/payouts"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return CreateResult{}, fmt.Errorf("build create payout request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", req.TransferID+":"+strconv.Itoa(req.Attempt))
	if req.Scenario != "" {
		httpReq.Header.Set("X-Scenario", req.Scenario)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return CreateResult{Outcome: OutcomeUnknown, Detail: err.Error()}, nil
	}
	defer resp.Body.Close()
	return classifyCreateResponse(resp)
}

func (c *Client) GetPayout(ctx context.Context, providerID, payoutID string) (Payout, bool, error) {
	if strings.TrimSpace(c.baseURL) == "" {
		return Payout{}, false, errors.New("base URL is empty")
	}
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(payoutID) == "" {
		return Payout{}, false, errors.New("provider id or payout id is empty")
	}
	endpoint := c.baseURL + "/" + url.PathEscape(providerID) + "/payouts/" + url.PathEscape(payoutID)
	return c.doGetPayout(ctx, endpoint)
}

func (c *Client) FindPayout(ctx context.Context, providerID, reference string) (Payout, bool, error) {
	if strings.TrimSpace(c.baseURL) == "" {
		return Payout{}, false, errors.New("base URL is empty")
	}
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(reference) == "" {
		return Payout{}, false, errors.New("provider id or reference is empty")
	}
	endpoint := c.baseURL + "/" + url.PathEscape(providerID) + "/payouts?reference=" + url.QueryEscape(reference)
	return c.doGetPayout(ctx, endpoint)
}

func (c *Client) doGetPayout(ctx context.Context, endpoint string) (Payout, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Payout{}, false, fmt.Errorf("build get payout request: %w", err)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Payout{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Payout{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Payout{}, false, fmt.Errorf("unexpected status %d from provider", resp.StatusCode)
	}
	body, err := readBody(resp.Body)
	if err != nil {
		return Payout{}, false, fmt.Errorf("read payout response: %w", err)
	}
	var raw struct {
		PayoutID    string  `json:"payout_id"`
		Reference   string  `json:"reference"`
		Status      string  `json:"status"`
		FailureCode *string `json:"failure_code"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Payout{}, false, fmt.Errorf("decode payout response: %w", err)
	}
	if strings.TrimSpace(raw.PayoutID) == "" {
		return Payout{}, false, errors.New("payout id is missing")
	}
	p := Payout{
		PayoutID:  raw.PayoutID,
		Reference: raw.Reference,
		Status:    raw.Status,
	}
	if raw.FailureCode != nil {
		p.FailureCode = *raw.FailureCode
	}
	return p, true, nil
}

func validateCreate(baseURL, publicURL string, req CreateRequest) error {
	if strings.TrimSpace(baseURL) == "" {
		return errors.New("base URL is empty")
	}
	if strings.TrimSpace(publicURL) == "" {
		return errors.New("public URL is empty")
	}
	if strings.TrimSpace(req.ProviderID) == "" {
		return errors.New("provider id is empty")
	}
	if strings.TrimSpace(req.TransferID) == "" {
		return errors.New("transfer id is empty")
	}
	if strings.TrimSpace(req.Amount) == "" {
		return errors.New("amount is empty")
	}
	if strings.TrimSpace(req.Currency) == "" {
		return errors.New("currency is empty")
	}
	if req.Attempt < 1 {
		return errors.New("attempt must be at least 1")
	}
	trimmed := bytes.TrimSpace(req.Recipient)
	if len(trimmed) == 0 {
		return errors.New("recipient is empty")
	}
	if !json.Valid(trimmed) {
		return errors.New("recipient is not valid json")
	}
	if trimmed[0] != '{' {
		return errors.New("recipient must be a json object")
	}
	switch req.Scenario {
	case "", "ok", "decline", "error_5xx", "slow":
	default:
		return errors.New("scenario is invalid")
	}
	return nil
}

func encodeCreateBody(publicURL string, req CreateRequest) ([]byte, error) {
	payload := struct {
		Reference   string          `json:"reference"`
		Amount      string          `json:"amount"`
		Currency    string          `json:"currency"`
		Recipient   json.RawMessage `json:"recipient"`
		CallbackURL string          `json:"callback_url"`
	}{
		Reference:   req.TransferID,
		Amount:      req.Amount,
		Currency:    req.Currency,
		Recipient:   json.RawMessage(bytes.TrimSpace(req.Recipient)),
		CallbackURL: strings.TrimRight(publicURL, "/") + "/webhooks/" + url.PathEscape(req.ProviderID),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("encode payout body: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func classifyCreateResponse(resp *http.Response) (CreateResult, error) {
	body, err := readBody(resp.Body)
	if err != nil {
		return CreateResult{Outcome: OutcomeUnknown, Detail: err.Error()}, nil
	}
	switch resp.StatusCode {
	case http.StatusCreated:
		var ok struct {
			PayoutID string `json:"payout_id"`
			Status   string `json:"status"`
		}
		if err := json.Unmarshal(body, &ok); err != nil || strings.TrimSpace(ok.PayoutID) == "" {
			return CreateResult{Outcome: OutcomeUnknown, Detail: "created response has no payout id"}, nil
		}
		return CreateResult{
			Outcome:  OutcomeCreated,
			PayoutID: ok.PayoutID,
			Status:   ok.Status,
		}, nil
	case http.StatusUnprocessableEntity:
		var declined struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &declined)
		return CreateResult{Outcome: OutcomeDeclined, Code: declined.Code}, nil
	case http.StatusInternalServerError, http.StatusServiceUnavailable:
		var pe struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &pe)
		return CreateResult{Outcome: OutcomeProviderError, Code: pe.Code}, nil
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		return CreateResult{Outcome: OutcomeUnknown, Detail: fmt.Sprintf("status %d", resp.StatusCode)}, nil
	default:
		return CreateResult{}, fmt.Errorf("provider returned status %d", resp.StatusCode)
	}
}

func readBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxProviderBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxProviderBody {
		return nil, errors.New("provider response is too large")
	}
	return body, nil
}
