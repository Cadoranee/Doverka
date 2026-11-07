package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Cadoranee/Doverka/internal/idempotency"
	"github.com/Cadoranee/Doverka/internal/ledger"
	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/jackc/pgx/v5"
)

type clockNow interface {
	Now() time.Time
}

type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type transferCreator interface {
	Create(ctx context.Context, tx pgx.Tx, nt transfer.NewTransfer) (transfer.Transfer, error)
	Transition(ctx context.Context, tx pgx.Tx, transferID string, to transfer.Status, reason string) (transfer.Transition, error)
}

type balanceReserver interface {
	LockBalance(ctx context.Context, tx pgx.Tx, clientID, currency string) (ledger.Balance, error)
	Reserve(ctx context.Context, tx pgx.Tx, clientID, currency, transferID string, amountMinor int64) error
}

type idempotencyRecorder interface {
	Begin(ctx context.Context, tx pgx.Tx, key string, body []byte) (idempotency.Decision, error)
	Save(ctx context.Context, tx pgx.Tx, key string, statusCode int, body []byte) error
}

type createTransferRequest struct {
	QuoteID   string          `json:"quote_id"`
	ClientID  string          `json:"client_id"`
	Recipient json.RawMessage `json:"recipient"`
}

type createTransferResponse struct {
	TransferID string `json:"transfer_id"`
	Status     string `json:"status"`
}

func (h *Handler) serveCreateTransfer(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Store == nil || h.DB == nil || h.Transfers == nil || h.Ledger == nil || h.Idempotency == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "VALIDATION_ERROR")
		return
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	req, ok := parseCreateTransferRequest(rawBody)
	if !ok {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "VALIDATION_ERROR")
		return
	}

	scenario := strings.TrimSpace(r.Header.Get("X-Scenario"))
	if !allowedTransferScenario(scenario) {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "VALIDATION_ERROR")
		return
	}

	status, body, transferID, err := h.createTransfer(r.Context(), idempotencyKey, rawBody, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	writeRawJSON(w, status, body)
	if transferID != "" && h.Runner != nil {
		go func(tid, scen string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := h.Runner.Run(ctx, tid, scen); err != nil {
				log.Printf("transfer %s run: %v", tid, err)
			}
		}(transferID, scenario)
	}
}

func allowedTransferScenario(scenario string) bool {
	switch scenario {
	case "", "ok", "decline", "error_5xx", "slow":
		return true
	default:
		return false
	}
}

func (h *Handler) createTransfer(ctx context.Context, idempotencyKey string, rawBody []byte, req createTransferRequest) (int, []byte, string, error) {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return 0, nil, "", err
	}
	defer tx.Rollback(ctx)

	dec, err := h.Idempotency.Begin(ctx, tx, idempotencyKey, rawBody)
	if err != nil {
		if errors.Is(err, idempotency.ErrReused) {
			body, mErr := marshalAPIError("IDEMPOTENCY_KEY_REUSED", "IDEMPOTENCY_KEY_REUSED")
			if mErr != nil {
				return 0, nil, "", mErr
			}
			return http.StatusConflict, body, "", nil
		}
		if errors.Is(err, idempotency.ErrInProgress) {
			body, mErr := marshalAPIError("INTERNAL", "internal error")
			if mErr != nil {
				return 0, nil, "", mErr
			}
			return http.StatusServiceUnavailable, body, "", nil
		}
		return 0, nil, "", err
	}
	if dec.Replay != nil {
		if err := tx.Commit(ctx); err != nil {
			return 0, nil, "", err
		}
		return dec.Replay.StatusCode, dec.Replay.Body, "", nil
	}

	q, err := h.Store.Get(ctx, req.QuoteID)
	if err != nil {
		if errors.Is(err, quote.ErrNotFound) {
			status, body, cerr := h.commitTransferError(ctx, tx, idempotencyKey, http.StatusNotFound, "QUOTE_NOT_FOUND")
			return status, body, "", cerr
		}
		return 0, nil, "", err
	}
	if q.Expired(h.handlerNow()) {
		status, body, cerr := h.commitTransferError(ctx, tx, idempotencyKey, http.StatusUnprocessableEntity, "QUOTE_EXPIRED")
		return status, body, "", cerr
	}

	bal, err := h.Ledger.LockBalance(ctx, tx, req.ClientID, q.SourceCurrency)
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			status, body, cerr := h.commitTransferError(ctx, tx, idempotencyKey, http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS")
			return status, body, "", cerr
		}
		return 0, nil, "", err
	}
	if bal.Available < q.SourceAmountMinor {
		status, body, cerr := h.commitTransferError(ctx, tx, idempotencyKey, http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS")
		return status, body, "", cerr
	}

	transferID, err := newTransferID()
	if err != nil {
		return 0, nil, "", err
	}
	recipient := append(json.RawMessage(nil), req.Recipient...)
	_, err = h.Transfers.Create(ctx, tx, transfer.NewTransfer{
		ID:                transferID,
		QuoteID:           q.ID,
		ClientID:          req.ClientID,
		SourceAmountMinor: q.SourceAmountMinor,
		SourceCurrency:    q.SourceCurrency,
		TargetAmountMinor: q.TargetAmountMinor,
		TargetCurrency:    q.TargetCurrency,
		Recipient:         recipient,
	})
	if err != nil {
		return 0, nil, "", err
	}
	if _, err := h.Transfers.Transition(ctx, tx, transferID, transfer.StatusReserved, "reserve"); err != nil {
		return 0, nil, "", err
	}
	if err := h.Ledger.Reserve(ctx, tx, req.ClientID, q.SourceCurrency, transferID, q.SourceAmountMinor); err != nil {
		return 0, nil, "", err
	}

	successBody, err := json.Marshal(createTransferResponse{
		TransferID: transferID,
		Status:     string(transfer.StatusReserved),
	})
	if err != nil {
		return 0, nil, "", err
	}
	if err := h.Idempotency.Save(ctx, tx, idempotencyKey, http.StatusAccepted, successBody); err != nil {
		return 0, nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, "", err
	}
	return http.StatusAccepted, successBody, transferID, nil
}

func (h *Handler) handlerNow() time.Time {
	if h.Clock != nil {
		return h.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *Handler) commitTransferError(ctx context.Context, tx pgx.Tx, key string, status int, code string) (int, []byte, error) {
	body, err := marshalAPIError(code, code)
	if err != nil {
		return 0, nil, err
	}
	if err := h.Idempotency.Save(ctx, tx, key, status, body); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return status, body, nil
}

func parseCreateTransferRequest(rawBody []byte) (createTransferRequest, bool) {
	var req createTransferRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		return createTransferRequest{}, false
	}
	if strings.TrimSpace(req.QuoteID) == "" || strings.TrimSpace(req.ClientID) == "" {
		return createTransferRequest{}, false
	}
	if !recipientIsObject(req.Recipient) {
		return createTransferRequest{}, false
	}
	return req, true
}

func recipientIsObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '{' {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}

func marshalAPIError(code, message string) ([]byte, error) {
	var body errorResponse
	body.Error.Code = code
	body.Error.Message = message
	return json.Marshal(body)
}

func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func newTransferID() (string, error) {
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
