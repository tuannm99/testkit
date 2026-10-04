// Package search indexes orders into Elasticsearch with _bulk. A 200 answer
// may still carry per-item failures ("errors": true); they are errors here.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
)

type ES struct {
	URL   string
	Index string
	HTTP  *http.Client
	Log   *slog.Logger
}

type bulkResp struct {
	Errors bool `json:"errors"`
	Items  []map[string]struct {
		Status int `json:"status"`
		Error  *struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	} `json:"items"`
}

// IndexOrders upserts order documents (document id = order id, so retries are idempotent).
func (e *ES) IndexOrders(ctx context.Context, orders ...domain.Order) error {
	if e == nil || e.URL == "" {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, o := range orders {
		_ = enc.Encode(map[string]any{"index": map[string]any{"_index": e.Index, "_id": o.ID}})
		_ = enc.Encode(map[string]any{"order_id": o.ID, "status": o.Status, "amount_cents": o.AmountCents,
			"currency": o.Currency, "customer_email": o.CustomerEmail, "payment_ref": o.PaymentRef, "updated_at": o.UpdatedAt})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL+"/_bulk", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("es bulk: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("es bulk: HTTP %d: %.300s", resp.StatusCode, raw)
	}
	var br bulkResp
	if err := json.Unmarshal(raw, &br); err != nil {
		return fmt.Errorf("es bulk: decode: %w", err)
	}
	if !br.Errors {
		return nil
	}
	var first string
	failed := 0
	for _, it := range br.Items {
		for _, r := range it {
			if r.Error != nil {
				failed++
				if first == "" {
					first = r.Error.Type + ": " + r.Error.Reason
				}
			}
		}
	}
	if failpoint.Enabled(failpoint.ESIgnoreBulkErrors) {
		e.Log.Warn("failpoint: ignoring es bulk item errors", "failed", failed, "first", first)
		return nil
	}
	return fmt.Errorf("es bulk: %d/%d items failed (HTTP 200): %s", failed, len(br.Items), first)
}
