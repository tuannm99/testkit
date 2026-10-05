// Package mail sends the payment confirmation over SMTP.
package mail

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/tuannm99/testkit/reference-worker/internal/domain"
)

type SMTP struct {
	Addr string
	From string
}

func (s *SMTP) Enabled() bool { return s != nil && s.Addr != "" }

// SendPaid sends the "payment received" mail.
func (s *SMTP) SendPaid(ctx context.Context, o domain.Order) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	host, _, _ := net.SplitHostPort(s.Addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	defer c.Close()
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("smtp MAIL: %w", err)
	}
	if err := c.Rcpt(o.CustomerEmail); err != nil {
		return fmt.Errorf("smtp RCPT: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	msg := strings.Join([]string{
		"From: " + s.From,
		"To: " + o.CustomerEmail,
		"Subject: Payment received for order " + o.ID,
		"Message-ID: <order-" + o.ID + "@shop.test>",
		"Content-Type: text/plain; charset=utf-8",
		"",
		fmt.Sprintf("We received %d %s for order %s (ref %s).", o.AmountCents, o.Currency, o.ID, o.PaymentRef),
		"",
	}, "\r\n")
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp end of DATA: %w", err)
	}
	return c.Quit()
}
