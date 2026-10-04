-- Payment provider webhooks: refunds notified asynchronously.
ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check
    CHECK (status IN ('pending', 'processing', 'paid', 'failed', 'refunded'));
ALTER TABLE orders ADD COLUMN last_event_seq bigint NOT NULL DEFAULT 0;

-- Every webhook event is recorded once (idempotency by provider event id).
CREATE TABLE payment_events (
    event_id    text PRIMARY KEY,
    order_id    text        NOT NULL,
    type        text        NOT NULL,
    seq         bigint      NOT NULL,
    applied     boolean     NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);
