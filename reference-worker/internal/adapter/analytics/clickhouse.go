// Package analytics writes order events to ClickHouse over HTTP. Every insert
// carries insert_deduplication_token = event id, so a retried job does not
// create duplicate rows.
package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tuannm99/testkit/reference-worker/internal/domain"
	"github.com/tuannm99/testkit/reference-worker/internal/failpoint"
)

type ClickHouse struct {
	URL      string
	Database string
	User     string
	Password string
	HTTP     *http.Client
}

func (c *ClickHouse) Enabled() bool { return c != nil && c.URL != "" }

// Event writes one analytics row (idempotent by event id).
func (c *ClickHouse) Event(ctx context.Context, eventID, typ string, o domain.Order) error {
	if !c.Enabled() {
		return nil
	}
	row, _ := json.Marshal(map[string]any{"event_id": eventID, "order_id": o.ID, "type": typ,
		"amount_cents": o.AmountCents, "currency": o.Currency, "at": time.Now().UTC().Format("2006-01-02 15:04:05.000")})
	q := url.Values{}
	q.Set("database", c.Database)
	q.Set("query", "INSERT INTO order_events FORMAT JSONEachRow")
	if !failpoint.Enabled(failpoint.CHNoDedupToken) {
		q.Set("insert_deduplication_token", eventID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/?"+q.Encode(), bytes.NewReader(row))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Password)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("clickhouse insert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("clickhouse insert: HTTP %d: %.300s", resp.StatusCode, b)
	}
	return nil
}
