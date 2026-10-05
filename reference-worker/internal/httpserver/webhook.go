package httpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tuannm99/testkit/reference-worker/internal/adapter/pgrepo"
	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
)

// WebhookHandler receives provider events: signature checked
// (X-Signature: sha256=<hex hmac>), schema version checked, idempotent by
// event id, out-of-order events recorded but not applied.
type WebhookHandler struct {
	Secret string
	Repo   *pgrepo.Repo
	Log    *slog.Logger
	// Read models refreshed when a refund is applied.
	Cache interface {
		SetStatus(ctx context.Context, orderID, status string) error
	}
	Search interface {
		IndexOrders(ctx context.Context, orders ...domain.Order) error
	}
}

type event struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	APIVersion string `json:"api_version"`
	Seq        int64  `json:"seq"`
	Data       struct {
		OrderID string `json:"order_id"`
	} `json:"data"`
}

// Supported provider payload versions.
var supportedVersions = map[string]bool{"2024-06-01": true}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	m := hmac.New(sha256.New, []byte(h.Secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if h.Secret == "" || !hmac.Equal([]byte(r.Header.Get("X-Signature")), []byte(want)) {
		h.Log.Warn("webhook rejected: bad signature")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.Data.OrderID == "" {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if !supportedVersions[ev.APIVersion] {
		h.Log.Warn("webhook rejected: unsupported payload version", "event_id", ev.ID, "api_version", ev.APIVersion)
		http.Error(w, "unsupported api_version", http.StatusUnprocessableEntity)
		return
	}
	if ev.Type != "charge.refunded" {
		w.WriteHeader(http.StatusAccepted) // other events are ignored
		return
	}
	applied, err := h.Repo.ApplyRefund(r.Context(), ev.ID, ev.Data.OrderID, ev.Seq)
	switch {
	case errors.Is(err, pgrepo.ErrDuplicateEvent):
		h.Log.Info("webhook duplicate ignored", "event_id", ev.ID)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, pgx.ErrNoRows):
		http.Error(w, "unknown order", http.StatusNotFound)
	case err != nil:
		h.Log.Error("webhook", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		h.Log.Info("webhook processed", "event_id", ev.ID, "order_id", ev.Data.OrderID, "seq", ev.Seq, "applied", applied)
		if applied {
			if err := h.refreshReadModels(r.Context(), ev.Data.OrderID); err != nil {
				// The refund is committed; read models are refreshed again by the next event.
				h.Log.Error("refresh read models after refund", "order_id", ev.Data.OrderID, "err", err.Error())
			}
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (h *WebhookHandler) refreshReadModels(ctx context.Context, orderID string) error {
	o, err := h.Repo.Get(ctx, orderID)
	if err != nil {
		return err
	}
	if h.Cache != nil && !failpoint.Enabled(failpoint.SkipCacheInvalidate) {
		if err := h.Cache.SetStatus(ctx, o.ID, string(o.Status)); err != nil {
			return err
		}
	}
	if h.Search != nil {
		return h.Search.IndexOrders(ctx, o)
	}
	return nil
}
