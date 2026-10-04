package pgrepo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrDuplicateEvent: the provider event was already recorded.
var ErrDuplicateEvent = errors.New("duplicate event")

// ApplyRefund records a provider event and applies it when it is newer than
// the last applied event of the order (out-of-order deliveries are recorded
// but not applied). It returns whether the event changed the order.
func (r *Repo) ApplyRefund(ctx context.Context, eventID, orderID string, seq int64) (bool, error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var lastSeq int64
	var status string
	err = tx.QueryRow(ctx, `SELECT last_event_seq, status FROM orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&lastSeq, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err != nil {
		return false, err
	}
	apply := seq > lastSeq && (status == "paid" || status == "refunded")
	tag, err := tx.Exec(ctx, `INSERT INTO payment_events (event_id, order_id, type, seq, applied) VALUES ($1, $2, 'charge.refunded', $3, $4)
		ON CONFLICT (event_id) DO NOTHING`, eventID, orderID, seq, apply)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, ErrDuplicateEvent
	}
	if apply {
		if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'refunded', last_event_seq = $2, updated_at = now() WHERE id = $1`, orderID, seq); err != nil {
			return false, err
		}
	}
	return apply, tx.Commit(ctx)
}
