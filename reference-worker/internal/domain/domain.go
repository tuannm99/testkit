// Package domain holds the business types of the order worker.
package domain

import (
	"errors"
	"time"
)

type OrderStatus string

const (
	StatusPending    OrderStatus = "pending"
	StatusProcessing OrderStatus = "processing"
	StatusPaid       OrderStatus = "paid"
	StatusFailed     OrderStatus = "failed"
)

type Order struct {
	ID            string      `json:"id"`
	CustomerEmail string      `json:"customer_email"`
	AmountCents   int64       `json:"amount_cents"`
	Currency      string      `json:"currency"`
	Status        OrderStatus `json:"status"`
	PaymentRef    string      `json:"payment_ref,omitempty"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// Job is what both triggers hand to Process: a Kafka record or a jobs row.
type Job struct {
	ID      string // job key (Kafka: job_id field; DB: job_key column)
	OrderID string
	Source  string // kafka | dbpoll
	Attempt int
	// Delivery identifies this physical delivery (Kafka topic/partition/offset,
	// DB job row id). Two deliveries of the same job key must never share it.
	Delivery string
}

// Charge is the payment gateway answer.
type Charge struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

var (
	// ErrPermanent wraps errors that retrying cannot fix (poison messages,
	// declined payments, unknown orders). Triggers dead-letter them.
	ErrPermanent = errors.New("permanent")
	// ErrBusy means another worker is processing the same order right now.
	ErrBusy     = errors.New("order is being processed by another worker")
	ErrNotFound = errors.New("not found")
	// ErrUnavailable: a dependency the worker cannot work without (its
	// database) is unreachable. Retrying later is the only option and the
	// job itself is not at fault, so it must not consume attempts nor be
	// dead-lettered.
	ErrUnavailable = errors.New("dependency unavailable")
)

// Unavailable marks err as an infrastructure outage.
func Unavailable(err error) error { return &unavailableErr{err} }

type unavailableErr struct{ err error }

func (u *unavailableErr) Error() string   { return "unavailable: " + u.err.Error() }
func (u *unavailableErr) Unwrap() []error { return []error{u.err, ErrUnavailable} }

// IsUnavailable reports whether err is an infrastructure outage.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

// Permanent marks err as non-retryable.
func Permanent(err error) error { return &permanentErr{err} }

type permanentErr struct{ err error }

func (p *permanentErr) Error() string   { return "permanent: " + p.err.Error() }
func (p *permanentErr) Unwrap() []error { return []error{p.err, ErrPermanent} }

// IsPermanent reports whether err must not be retried.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }
