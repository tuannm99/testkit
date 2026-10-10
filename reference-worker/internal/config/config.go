// Package config reads the worker configuration from the environment.
// Every address and credential is injected by the deployment (TestKit in tests).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ServiceName string
	RunID       string
	HTTPAddr    string
	WorkerID    string

	DatabaseURL string

	Triggers []string // kafka, dbpoll, rabbitmq, redis

	KafkaBrokers     []string
	KafkaTopic       string
	KafkaGroup       string
	KafkaDLQTopic    string
	KafkaMaxAttempts int
	KafkaSession     time.Duration

	RabbitURL      string
	RabbitQueue    string
	RabbitPrefetch int
	RabbitAttempts int

	RedisQueue       string // full key of the Redis queue trigger
	RedisQueueKind   string // stream | list
	RedisGroup       string
	RedisProcessing  string
	RedisDLQ         string
	RedisClaimIdle   time.Duration
	RedisMaxAttempts int

	PollInterval    time.Duration
	PollMaxIdle     time.Duration
	PollBatch       int
	PollLease       time.Duration
	PollConcurrency int

	PaymentURL         string
	PaymentTimeout     time.Duration
	PaymentMaxAttempts int
	PaymentMaxBackoff  time.Duration

	ESURL   string
	ESIndex string

	SMTPAddr string
	MailFrom string

	OutboxTopic    string
	OutboxInterval time.Duration

	OrderLease time.Duration

	WebhookSecret string

	CHURL      string
	CHDatabase string
	CHUser     string
	CHPassword string

	MongoURI string
	MongoDB  string

	RedisAddr   string
	RedisPrefix string

	ESHistoryIndex string

	PartnerWSURL string
	WSHeartbeat  time.Duration
}

func Load() (Config, error) {
	var errs []string
	c := Config{
		ServiceName:        get("SERVICE_NAME", "order-worker"),
		RunID:              get("TESTKIT_RUN_ID", ""),
		HTTPAddr:           get("HTTP_ADDR", ":8080"),
		DatabaseURL:        get("DATABASE_URL", ""),
		Triggers:           list(get("TRIGGERS", "kafka,dbpoll")),
		KafkaBrokers:       list(get("KAFKA_BROKERS", "")),
		KafkaTopic:         get("KAFKA_TOPIC", ""),
		KafkaGroup:         get("KAFKA_GROUP", ""),
		KafkaDLQTopic:      get("KAFKA_DLQ_TOPIC", ""),
		KafkaMaxAttempts:   num("KAFKA_MAX_ATTEMPTS", 5, &errs),
		KafkaSession:       dur("KAFKA_SESSION_TIMEOUT", 45*time.Second, &errs),
		RabbitURL:          get("RABBITMQ_URL", ""),
		RabbitQueue:        get("RABBITMQ_QUEUE", ""),
		RabbitPrefetch:     num("RABBITMQ_PREFETCH", 10, &errs),
		RabbitAttempts:     num("RABBITMQ_MAX_ATTEMPTS", 5, &errs),
		RedisQueue:         get("REDIS_QUEUE", ""),
		RedisQueueKind:     get("REDIS_QUEUE_KIND", "stream"),
		RedisGroup:         get("REDIS_QUEUE_GROUP", ""),
		RedisProcessing:    get("REDIS_QUEUE_PROCESSING", ""),
		RedisDLQ:           get("REDIS_QUEUE_DLQ", ""),
		RedisClaimIdle:     dur("REDIS_QUEUE_CLAIM_IDLE", 5*time.Second, &errs),
		RedisMaxAttempts:   num("REDIS_QUEUE_MAX_ATTEMPTS", 5, &errs),
		PollInterval:       dur("POLL_INTERVAL", 200*time.Millisecond, &errs),
		PollMaxIdle:        dur("POLL_MAX_IDLE", 2*time.Second, &errs),
		PollBatch:          num("POLL_BATCH", 10, &errs),
		PollLease:          dur("POLL_LEASE", 15*time.Second, &errs),
		PollConcurrency:    num("POLL_CONCURRENCY", 4, &errs),
		PaymentURL:         get("PAYMENT_URL", ""),
		PaymentTimeout:     dur("PAYMENT_TIMEOUT", 5*time.Second, &errs),
		PaymentMaxAttempts: num("PAYMENT_MAX_ATTEMPTS", 5, &errs),
		PaymentMaxBackoff:  dur("PAYMENT_MAX_BACKOFF", 10*time.Second, &errs),
		ESURL:              get("ES_URL", ""),
		ESIndex:            get("ES_INDEX", ""),
		SMTPAddr:           get("SMTP_ADDR", ""),
		MailFrom:           get("MAIL_FROM", "orders@shop.test"),
		OutboxTopic:        get("OUTBOX_TOPIC", ""),
		OutboxInterval:     dur("OUTBOX_INTERVAL", 200*time.Millisecond, &errs),
		OrderLease:         dur("ORDER_LEASE", 30*time.Second, &errs),
		WebhookSecret:      get("WEBHOOK_SECRET", ""),
		CHURL:              get("CH_URL", ""),
		CHDatabase:         get("CH_DATABASE", "default"),
		CHUser:             get("CH_USER", "default"),
		CHPassword:         get("CH_PASSWORD", ""),
		MongoURI:           get("MONGO_URI", ""),
		MongoDB:            get("MONGO_DB", "orders"),
		RedisAddr:          get("REDIS_ADDR", ""),
		RedisPrefix:        get("REDIS_PREFIX", ""),
		ESHistoryIndex:     get("ES_HISTORY_INDEX", ""),
		PartnerWSURL:       get("PARTNER_WS_URL", ""),
		WSHeartbeat:        dur("WS_HEARTBEAT", time.Second, &errs),
	}
	host, _ := os.Hostname()
	c.WorkerID = get("WORKER_ID", host)
	if c.DatabaseURL == "" {
		errs = append(errs, "DATABASE_URL is required")
	}
	if c.PaymentURL == "" {
		errs = append(errs, "PAYMENT_URL is required")
	}
	if c.Has("kafka") && (len(c.KafkaBrokers) == 0 || c.KafkaTopic == "" || c.KafkaGroup == "") {
		errs = append(errs, "trigger kafka needs KAFKA_BROKERS, KAFKA_TOPIC, KAFKA_GROUP")
	}
	if c.Has("rabbitmq") && (c.RabbitURL == "" || c.RabbitQueue == "") {
		errs = append(errs, "trigger rabbitmq needs RABBITMQ_URL, RABBITMQ_QUEUE")
	}
	if c.Has("redis") {
		switch {
		case c.RedisAddr == "" || c.RedisQueue == "":
			errs = append(errs, "trigger redis needs REDIS_ADDR, REDIS_QUEUE")
		case c.RedisQueueKind == "stream" && c.RedisGroup == "":
			errs = append(errs, "redis stream trigger needs REDIS_QUEUE_GROUP")
		case c.RedisQueueKind == "list" && c.RedisProcessing == "":
			errs = append(errs, "redis list trigger needs REDIS_QUEUE_PROCESSING")
		case c.RedisQueueKind != "stream" && c.RedisQueueKind != "list":
			errs = append(errs, "REDIS_QUEUE_KIND must be stream or list")
		}
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

func (c Config) Has(trigger string) bool {
	for _, t := range c.Triggers {
		if t == trigger {
			return true
		}
	}
	return false
}

func get(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func num(k string, def int, errs *[]string) int {
	v := get(k, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, k+": "+err.Error())
	}
	return n
}

func dur(k string, def time.Duration, errs *[]string) time.Duration {
	v := get(k, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, k+": "+err.Error())
	}
	return d
}
