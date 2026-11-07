package httpapi

import (
	"context"
	"net/http"
	"sort"

	"github.com/Cadoranee/Doverka/internal/ledger"
)

type BalanceLister interface {
	List(ctx context.Context, clientID string) ([]ledger.Balance, error)
}

type clientBalanceResponse struct {
	ClientID string                `json:"client_id"`
	Balances []balanceItemResponse `json:"balances"`
}

type balanceItemResponse struct {
	Currency  string `json:"currency"`
	Available string `json:"available"`
	Reserved  string `json:"reserved"`
}

func (h *Handler) serveClientBalance(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Catalog == nil || h.Balances == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	clientID := r.PathValue("client_id")
	if _, ok := h.Catalog.Client(clientID); !ok {
		writeError(w, http.StatusNotFound, "CLIENT_NOT_FOUND", "CLIENT_NOT_FOUND")
		return
	}
	rows, err := h.Balances.List(r.Context(), clientID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	items := make([]balanceItemResponse, 0, len(rows))
	for _, b := range rows {
		cur, ok := h.Catalog.Currency(b.Currency)
		if !ok {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
			return
		}
		items = append(items, balanceItemResponse{
			Currency:  b.Currency,
			Available: minorToMoney(b.Available, cur.MinorUnits, b.Currency).Amount,
			Reserved:  minorToMoney(b.Reserved, cur.MinorUnits, b.Currency).Amount,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Currency < items[j].Currency
	})
	writeJSON(w, http.StatusOK, clientBalanceResponse{
		ClientID: clientID,
		Balances: items,
	})
}
