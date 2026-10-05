package httpserver

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// orderPage is the back-office view of one order (exercised by the UI tests).
var orderPage = template.Must(template.New("order").Parse(`<!doctype html>
<html lang="vi"><head><meta charset="utf-8"><title>Đơn {{.ID}}</title>
<style>body{font-family:sans-serif;margin:2rem}.badge{padding:.2rem .6rem;border-radius:1rem}
.paid{background:#d6f5df;color:#11622e}.pending{background:#fff1c2;color:#7a5a00}.failed,.refunded{background:#fde0e0;color:#8a1c1c}</style>
</head><body>
<h1>Đơn hàng <span data-testid="order-id">{{.ID}}</span></h1>
<p>Trạng thái: <span class="badge {{.Status}}" data-testid="order-status">{{.Label}}</span></p>
<p>Số tiền: <span data-testid="order-amount">{{.Amount}}</span></p>
<p>Mã thanh toán: <span data-testid="payment-ref">{{if .PaymentRef}}{{.PaymentRef}}{{else}}—{{end}}</span></p>
</body></html>`))

var statusLabel = map[string]string{"pending": "Chờ thanh toán", "paid": "Đã thanh toán", "failed": "Thanh toán lỗi", "refunded": "Đã hoàn tiền"}

func orderHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var v struct {
			ID, Status, Label, Amount, PaymentRef string
		}
		var cents int64
		var currency string
		var ref *string
		err := pool.QueryRow(ctx, `SELECT id, status, amount_cents, currency, payment_ref FROM orders WHERE id = $1`, r.PathValue("id")).
			Scan(&v.ID, &v.Status, &cents, &currency, &ref)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		v.Label = statusLabel[v.Status]
		if v.Label == "" {
			v.Label = v.Status
		}
		v.Amount = formatAmount(cents, currency)
		if ref != nil {
			v.PaymentRef = *ref
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = orderPage.Execute(w, v)
	}
}

func formatAmount(cents int64, currency string) string {
	return fmt.Sprintf("%s %d.%02d", currency, cents/100, cents%100)
}
