// Package pgrepo is the Postgres adapter: orders, the processing claim, the
// mail guard and the transactional outbox.
package pgrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tuannm99/testkit/reference-worker/internal/domain"
)

type Repo struct {
	Pool        *pgxpool.Pool
	OutboxTopic string
}

func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 20
	return pgxpool.NewWithConfig(ctx, cfg)
}

func (r *Repo) Get(ctx context.Context, id string) (domain.Order, error) {
	var o domain.Order
	var ref *string
	err := r.Pool.QueryRow(ctx, `SELECT id, customer_email, amount_cents, currency, status, payment_ref, updated_at
		FROM orders WHERE id = $1`, id).Scan(&o.ID, &o.CustomerEmail, &o.AmountCents, &o.Currency, &o.Status, &ref, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, fmt.Errorf("order %s: %w", id, domain.ErrNotFound)
	}
	if ref != nil {
		o.PaymentRef = *ref
	}
	return o, err
}

// Claim moves the order to processing for owner until the lease expires.
// It returns domain.ErrBusy when another owner holds a live claim, and the
// current order when it is already in a terminal state.
func (r *Repo) Claim(ctx context.Context, id, owner string, lease time.Duration) (domain.Order, bool, error) {
	tag, err := r.Pool.Exec(ctx, `UPDATE orders
		SET status = 'processing', processing_by = $2, processing_until = now() + $3::interval, updated_at = now()
		WHERE id = $1 AND (status = 'pending' OR (status = 'processing' AND (processing_until < now() OR processing_by = $2)))`,
		id, owner, lease.String())
	if err != nil {
		return domain.Order{}, false, err
	}
	o, err := r.Get(ctx, id)
	if err != nil {
		return o, false, err
	}
	if tag.RowsAffected() == 1 {
		return o, true, nil
	}
	if o.Status == domain.StatusProcessing {
		return o, false, domain.ErrBusy
	}
	return o, false, nil
}

// Release puts a claimed order back to pending (processing failed, retry later).
func (r *Repo) Release(ctx context.Context, id, owner string) error {
	_, err := r.Pool.Exec(ctx, `UPDATE orders SET status = 'pending', processing_by = NULL, processing_until = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing' AND processing_by = $2`, id, owner)
	return err
}

// MarkFailed records a permanent payment failure.
func (r *Repo) MarkFailed(ctx context.Context, id, owner, reason string) error {
	_, err := r.Pool.Exec(ctx, `UPDATE orders SET status = 'failed', payment_ref = $3, processing_by = NULL, processing_until = NULL, updated_at = now()
		WHERE id = $1 AND processing_by = $2`, id, owner, reason)
	return err
}

// MarkPaid stores the payment and the order.paid outbox event atomically.
func (r *Repo) MarkPaid(ctx context.Context, o domain.Order, owner, chargeID string) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE orders SET status = 'paid', payment_ref = $3, processing_by = NULL, processing_until = NULL, updated_at = now()
		WHERE id = $1 AND processing_by = $2 AND status = 'processing'`, o.ID, owner, chargeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("order %s: claim lost before commit: %w", o.ID, domain.ErrBusy)
	}
	if r.OutboxTopic != "" {
		payload, _ := json.Marshal(map[string]any{"type": "order.paid", "order_id": o.ID, "payment_ref": chargeID,
			"amount_cents": o.AmountCents, "currency": o.Currency})
		if _, err := tx.Exec(ctx, `INSERT INTO outbox (event_id, topic, key, payload) VALUES ($1, $2, $3, $4)`,
			"order.paid:"+o.ID, r.OutboxTopic, o.ID, payload); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ClaimMail sets mail_sent_at once; only the caller that wins sends the mail.
func (r *Repo) ClaimMail(ctx context.Context, id string) (bool, error) {
	tag, err := r.Pool.Exec(ctx, `UPDATE orders SET mail_sent_at = now() WHERE id = $1 AND mail_sent_at IS NULL`, id)
	return tag.RowsAffected() == 1, err
}

// UnclaimMail reverts ClaimMail when sending failed.
func (r *Repo) UnclaimMail(ctx context.Context, id string) error {
	_, err := r.Pool.Exec(ctx, `UPDATE orders SET mail_sent_at = NULL WHERE id = $1`, id)
	return err
}
