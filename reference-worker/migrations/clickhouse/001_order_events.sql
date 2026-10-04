-- Analytics events. Inserts carry insert_deduplication_token, so a retried
-- insert of the same block is dropped (non-replicated tables need the window).
CREATE TABLE order_events
(
    event_id     String,
    order_id     String,
    type         LowCardinality(String),
    amount_cents Int64,
    currency     LowCardinality(String),
    at           DateTime64(3, 'UTC')
)
ENGINE = MergeTree
ORDER BY (order_id, at)
SETTINGS non_replicated_deduplication_window = 1000;
