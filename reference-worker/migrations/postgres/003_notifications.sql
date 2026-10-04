-- Partner notifications sent over WebSocket, at-least-once until acked.
CREATE TABLE notifications (
    id         bigserial PRIMARY KEY,
    msg_id     text        NOT NULL UNIQUE,
    payload    jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_count int         NOT NULL DEFAULT 0,
    acked_at   timestamptz
);
