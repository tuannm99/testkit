// Package failpoint lets test images break the worker on purpose: crash at a
// precise point, disable a safety mechanism (mutation testing). Failpoints are
// only compiled into images built with `-tags failpoint`; release images get
// the no-op implementation in off.go.
//
// TK_FAILPOINTS="name;name=once;name=always"
//
//	once   fires the first time only, remembered across container restarts
//	       through a marker file (the container filesystem survives a restart).
package failpoint

// Names used by the reference worker.
const (
	NoRetry             = "no_retry"              // classify every payment error as permanent
	NoIdempotencyKey    = "no_idempotency_key"    // omit the Idempotency-Key header
	SkipPaidCheck       = "skip_paid_check"       // charge even when the order is already paid
	SkipMailGuard       = "skip_mail_guard"       // send mail without the at-most-once guard
	CrashAfterDBCommit  = "crash_after_db_commit" // exit right after the paid transaction commits
	CommitBeforeProcess = "commit_before_process" // ack the Kafka record before processing
	ESIgnoreBulkErrors  = "es_ignore_bulk_errors" // treat a 200 _bulk with item errors as success
	CHNoDedupToken      = "ch_no_dedup_token"     // ClickHouse insert without deduplication token
	UseWorkerClock      = "use_worker_clock"      // compare run_at with the worker clock
	PollBusyLoop        = "poll_busy_loop"        // no idle backoff when the job table is empty
	MongoDupIsError     = "mongo_dup_is_error"    // treat E11000 (already recorded) as a failure
	SkipCacheInvalidate = "skip_cache_invalidate" // keep the cached status when a refund arrives
	WSNoResend          = "ws_no_resend"          // do not resend unacked notifications after reconnect
	WSNoHeartbeat       = "ws_no_heartbeat"       // never detect a silent (half-open) connection
)
