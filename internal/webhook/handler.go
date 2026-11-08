package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/ledger"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	eventStatusProcessing = "PROCESSING"
	eventStatusCompleted  = "COMPLETED"
	eventStatusFailed     = "FAILED"
)

type Handler struct {
	Secret    string
	Pool      *pgxpool.Pool
	Transfers *transfer.Machine
	Ledger    *ledger.Ledger
	Clock     clock.Clock
}

type webhookPayload struct {
	EventID    string `json:"event_id"`
	PayoutID   string `json:"payout_id"`
	Reference  string `json:"reference"`
	Status     string `json:"status"`
	OccurredAt string `json:"occurred_at"`
}

type payoutTransfer struct {
	clientID  string
	currency  string
	amount    int64
	status    transfer.Status
	createdAt time.Time
}

func (h *Handler) Handler() http.Handler {
	if !h.ready() {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/{provider_id}", h.serve)
	return mux
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	if !h.ready() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !verifySignature(h.Secret, r.Header.Get("X-Signature"), body) {
		w.WriteHeader(http.StatusOK)
		return
	}
	payload, ok := parsePayload(body)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := h.apply(r.Context(), r.PathValue("provider_id"), body, payload); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) apply(ctx context.Context, providerID string, body []byte, payload webhookPayload) error {
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin webhook tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tr, found, err := findTransfer(ctx, tx, payload.Reference)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	matched, err := payoutMatches(ctx, tx, payload.Reference, providerID, payload.PayoutID)
	if err != nil {
		return err
	}
	if !matched {
		return nil
	}
	if !advances(tr.status, payload.Status) {
		if !decisionFinal(tr.status) {
			return nil
		}
		if _, err := insertWebhookEvent(ctx, tx, providerID, payload.EventID, body, h.now()); err != nil {
			return err
		}
		return commit(ctx, tx)
	}

	if _, err := h.Ledger.LockBalance(ctx, tx, tr.clientID, tr.currency); err != nil {
		return err
	}
	locked, err := lockTransferStatus(ctx, tx, payload.Reference)
	if err != nil {
		return err
	}
	if !advances(locked, payload.Status) {
		if !decisionFinal(locked) {
			return nil
		}
		if _, err := insertWebhookEvent(ctx, tx, providerID, payload.EventID, body, h.now()); err != nil {
			return err
		}
		return commit(ctx, tx)
	}

	inserted, err := insertWebhookEvent(ctx, tx, providerID, payload.EventID, body, h.now())
	if err != nil {
		return err
	}
	if !inserted {
		return commit(ctx, tx)
	}
	if err := postLedger(ctx, tx, h.Ledger, tr, payload); err != nil {
		return err
	}
	to, reason := targetStatus(payload.Status)
	if _, err := h.Transfers.Transition(ctx, tx, payload.Reference, to, reason); err != nil {
		return err
	}
	if err := markAttempt(ctx, tx, payload.Reference, providerID, payload.PayoutID, string(to)); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func (h *Handler) ready() bool {
	return h != nil && h.Pool != nil && h.Ledger != nil && h.Transfers != nil
}

func (h *Handler) now() time.Time {
	if h != nil && h.Clock != nil {
		return h.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func verifySignature(secret, header string, body []byte) bool {
	sig, err := hex.DecodeString(strings.TrimSpace(header))
	if err != nil || len(sig) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

func parsePayload(body []byte) (webhookPayload, bool) {
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return webhookPayload{}, false
	}
	if payload.EventID == "" || payload.Reference == "" || payload.PayoutID == "" || !validEventStatus(payload.Status) {
		return webhookPayload{}, false
	}
	return payload, true
}

func validEventStatus(status string) bool {
	switch status {
	case eventStatusProcessing, eventStatusCompleted, eventStatusFailed:
		return true
	default:
		return false
	}
}

func advances(from transfer.Status, eventStatus string) bool {
	if from != transfer.StatusProcessing {
		return false
	}
	return eventStatus == eventStatusCompleted || eventStatus == eventStatusFailed
}

func decisionFinal(status transfer.Status) bool {
	return status == transfer.StatusProcessing || status.Terminal()
}

func targetStatus(eventStatus string) (transfer.Status, string) {
	if eventStatus == eventStatusFailed {
		return transfer.StatusFailed, "webhook failed"
	}
	return transfer.StatusCompleted, "webhook completed"
}

func postLedger(ctx context.Context, tx pgx.Tx, led *ledger.Ledger, tr payoutTransfer, payload webhookPayload) error {
	if payload.Status == eventStatusFailed {
		if err := led.Release(ctx, tx, tr.clientID, tr.currency, payload.Reference, tr.amount); err != nil {
			return err
		}
		return led.ReleaseDailyIfPresent(ctx, tx, tr.clientID, tr.createdAt, tr.currency, tr.amount)
	}
	return led.Debit(ctx, tx, tr.clientID, tr.currency, payload.Reference, tr.amount)
}

func findTransfer(ctx context.Context, tx pgx.Tx, transferID string) (payoutTransfer, bool, error) {
	var tr payoutTransfer
	var status string
	err := tx.QueryRow(ctx, `
SELECT client_id, source_amount_minor, source_currency, status, created_at
FROM transfers WHERE transfer_id = $1`,
		transferID,
	).Scan(&tr.clientID, &tr.amount, &tr.currency, &status, &tr.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return payoutTransfer{}, false, nil
	}
	if err != nil {
		return payoutTransfer{}, false, fmt.Errorf("select transfer: %w", err)
	}
	tr.status = transfer.Status(status)
	return tr, true, nil
}

func payoutMatches(ctx context.Context, tx pgx.Tx, transferID, providerID, payoutID string) (bool, error) {
	var matched bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS(
	SELECT 1 FROM provider_attempts
	WHERE transfer_id = $1 AND provider_id = $2 AND payout_id = $3
)`,
		transferID, providerID, payoutID,
	).Scan(&matched)
	if err != nil {
		return false, fmt.Errorf("select provider attempt: %w", err)
	}
	return matched, nil
}

func lockTransferStatus(ctx context.Context, tx pgx.Tx, transferID string) (transfer.Status, error) {
	var status string
	err := tx.QueryRow(ctx, `
SELECT status FROM transfers WHERE transfer_id = $1 FOR UPDATE`,
		transferID,
	).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("lock transfer: %w", err)
	}
	return transfer.Status(status), nil
}

func insertWebhookEvent(ctx context.Context, tx pgx.Tx, providerID, eventID string, body []byte, processedAt time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `
INSERT INTO webhook_events (provider_id, event_id, payload, processed_at)
VALUES ($1, $2, $3::jsonb, $4)
ON CONFLICT (provider_id, event_id) DO NOTHING`,
		providerID, eventID, string(body), processedAt,
	)
	if err != nil {
		return false, fmt.Errorf("insert webhook event: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func markAttempt(ctx context.Context, tx pgx.Tx, transferID, providerID, payoutID, result string) error {
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

func commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit webhook: %w", err)
	}
	return nil
}
