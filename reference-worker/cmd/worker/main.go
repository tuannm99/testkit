// Command worker is the TestKit reference worker: it pays orders received
// from Kafka or from a Postgres job table, calls a payment API, writes
// Postgres (+ outbox) and Elasticsearch, and mails the customer.
package main

import (
	"time"

	"go.uber.org/fx"

	"github.com/tuannm99/testkit/reference-worker/internal/app"
)

func main() {
	fx.New(app.Module, fx.StopTimeout(60*time.Second)).Run()
}
