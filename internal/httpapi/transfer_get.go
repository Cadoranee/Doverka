package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Cadoranee/Doverka/internal/transfer"
)

type transferView interface {
	Get(ctx context.Context, transferID string) (transfer.Transfer, error)
	History(ctx context.Context, transferID string) ([]transfer.Transition, error)
	Attempts(ctx context.Context, transferID string) ([]transfer.ProviderAttempt, error)
}

type transferDetailResponse struct {
	TransferID       string                   `json:"transfer_id"`
	Status           string                   `json:"status"`
	QuoteID          string                   `json:"quote_id"`
	ClientID         string                   `json:"client_id"`
	SourceAmount     moneyResponse            `json:"source_amount"`
	TargetAmount     moneyResponse            `json:"target_amount"`
	Transitions      []transferTransitionJSON `json:"transitions"`
	ProviderAttempts []transferAttemptJSON    `json:"provider_attempts"`
}

type transferTransitionJSON struct {
	From   string `json:"from"`
	To     string `json:"to"`
	At     string `json:"at"`
	Reason string `json:"reason"`
}

type transferAttemptJSON struct {
	ProviderID  string `json:"provider_id"`
	AttemptedAt string `json:"attempted_at"`
	Result      string `json:"result"`
	PayoutID    string `json:"payout_id"`
}

func (h *Handler) serveGetTransfer(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Catalog == nil || h.TransferView == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	id := r.PathValue("transfer_id")
	if id == "" {
		writeError(w, http.StatusNotFound, "TRANSFER_NOT_FOUND", "TRANSFER_NOT_FOUND")
		return
	}
	tr, err := h.TransferView.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, transfer.ErrNotFound) {
			writeError(w, http.StatusNotFound, "TRANSFER_NOT_FOUND", "TRANSFER_NOT_FOUND")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	hist, err := h.TransferView.History(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	attempts, err := h.TransferView.Attempts(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	sourceCur, ok := h.Catalog.Currency(tr.SourceCurrency)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	targetCur, ok := h.Catalog.Currency(tr.TargetCurrency)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	transitions := make([]transferTransitionJSON, 0, len(hist))
	for _, t := range hist {
		transitions = append(transitions, transferTransitionJSON{
			From:   string(t.From),
			To:     string(t.To),
			At:     t.At.UTC().Format(time.RFC3339),
			Reason: t.Reason,
		})
	}
	providerAttempts := make([]transferAttemptJSON, 0, len(attempts))
	for _, a := range attempts {
		providerAttempts = append(providerAttempts, transferAttemptJSON{
			ProviderID:  a.ProviderID,
			AttemptedAt: a.At.UTC().Format(time.RFC3339),
			Result:      a.Result,
			PayoutID:    a.PayoutID,
		})
	}
	resp := transferDetailResponse{
		TransferID:       tr.ID,
		Status:           string(tr.Status),
		QuoteID:          tr.QuoteID,
		ClientID:         tr.ClientID,
		SourceAmount:     minorToMoney(tr.SourceAmountMinor, sourceCur.MinorUnits, tr.SourceCurrency),
		TargetAmount:     minorToMoney(tr.TargetAmountMinor, targetCur.MinorUnits, tr.TargetCurrency),
		Transitions:      transitions,
		ProviderAttempts: providerAttempts,
	}
	writeJSON(w, http.StatusOK, resp)
}
