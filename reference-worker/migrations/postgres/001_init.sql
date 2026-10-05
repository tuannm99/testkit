-- Reference worker schema. Applied by TestKit into the per-run database.
CREATE TABLE orders (
    id               text PRIMARY KEY,
    customer_email   text        NOT NULL,
    amount_cents     bigint      NOT NULL CHECK (amount_cents > 0),
    currency         text        NOT NULL DEFAULT 'USD',
    status           text        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'processing', 'paid', 'failed')),
    payment_ref      text,
    processing_by    text,
    processing_until timestamptz,
    mail_sent_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Jobs for the DB-poll trigger. Claimed with FOR UPDATE SKIP LOCKED + lease.
-- Timestamps always come from the database clock (now()), never the worker's.
CREATE TABLE jobs (
    id           bigserial PRIMARY KEY,
    job_key      text        NOT NULL,
    order_id     text        NOT NULL,
    status       text        NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued', 'running', 'done', 'dead')),
    run_at       timestamptz NOT NULL DEFAULT now(),
    attempts     int         NOT NULL DEFAULT 0,
    max_attempts int         NOT NULL DEFAULT 5,
    locked_by    text,
    lease_until  timestamptz,
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_claim_idx ON jobs (status, run_at);

-- Transactional outbox: written in the same transaction as the state change,
-- relayed to Kafka afterwards (at-least-once, no "ghost" events).
CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    event_id     text        NOT NULL UNIQUE,
    topic        text        NOT NULL,
    key          text        NOT NULL,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_pending_idx ON outbox (id) WHERE published_at IS NULL;
