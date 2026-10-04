// Package steps is the shared step library: the only actions a scenario may
// use. Each step documents its parameters (validated by `testkit lint`) and
// names the connector that applies it. Register wires the adapters, steps
// and checks into a registry.
package steps

import (
	httpmock "github.com/tuannm99/testkit/testkit/adapters/mock/http"
	"github.com/tuannm99/testkit/testkit/adapters/mock/mail"
	"github.com/tuannm99/testkit/testkit/adapters/store/elasticsearch"
	"github.com/tuannm99/testkit/testkit/adapters/store/postgres"
	"github.com/tuannm99/testkit/testkit/adapters/sut"
	"github.com/tuannm99/testkit/testkit/adapters/trigger/dbpoll"
	"github.com/tuannm99/testkit/testkit/adapters/trigger/kafka"
	"github.com/tuannm99/testkit/testkit/core/kit"
)

var expectKeys = []string{"expect", "within", "check", "op", "expected", "why", "id",
	"eq", "ne", "gt", "gte", "lt", "lte", "in", "not_in", "contains", "not_contains", "matches",
	"exists", "not_exists", "between", "approx", "empty", "not_empty", "len_eq", "tolerance"}

// Defs is the step library.
var Defs = []kit.StepDef{
	// built-in (orchestrator)
	{Name: "trigger.enqueue", Doc: "Deliver a job through the execution's trigger (Kafka record or DB row); duplicate: N delivers it N times", Optional: []string{"id", "duplicate", "from"}, Open: true},
	{Name: "trigger.drain", Doc: "Wait until the trigger is drained (consumer lag 0 / no queued or running job)", Optional: []string{"timeout"}},
	{Name: "wait.until", Doc: "Poll a check until it holds (replaces sleep); fails the step after `within`", Optional: expectKeys},
	{Name: "assert", Doc: "Inline checkpoint: expectations that must hold at this point of the scenario", Optional: expectKeys},

	{Name: "postgres.insert", Connector: "postgres", Doc: "Insert fixture rows into an entity's table", Required: []string{"rows"}, Optional: []string{"entity", "table"}},
	{Name: "postgres.exec", Connector: "postgres", Doc: "Execute SQL in the namespace database", Required: []string{"sql"}, Optional: []string{"args"}},
	{Name: "postgres.query", Connector: "postgres", Doc: "Run a query and keep the rows as step output", Required: []string{"sql"}},

	{Name: "kafka.produce", Connector: "kafka", Doc: "Produce records to a namespaced topic", Required: []string{"topic", "value"}, Optional: []string{"key", "headers", "count"}},

	{Name: "es.insert", Connector: "elasticsearch", Doc: "Index fixture documents (refresh=true)", Required: []string{"entity", "rows"}},
	{Name: "es.refresh", Connector: "elasticsearch", Doc: "Refresh a namespaced index", Required: []string{"index"}},
	{Name: "es.block_writes", Connector: "elasticsearch", Doc: "Fault: block/unblock writes on an index (_bulk then returns 200 with item errors)", Required: []string{"index"}, Optional: []string{"enabled"}},

	{Name: "mock.script", Connector: "mock", Doc: "Script the responses of a mock (sequence, last repeats)", Required: []string{"mock"}, Optional: []string{"responses", "rules", "method", "path", "operation"}},
	{Name: "webhook.send", Connector: "mock", Doc: "Send a webhook to the service under test (signed/invalid/missing, duplicated, delayed)", Required: []string{"mock", "url", "body"}, Optional: []string{"method", "sign", "header", "repeat", "delay", "headers"}},

	{Name: "sut.restart", Connector: "sut", Doc: "Restart the service under test and wait until healthy", Optional: []string{"replica", "timeout"}},
	{Name: "sut.stop", Connector: "sut", Doc: "Graceful stop (SIGTERM, then SIGKILL after timeout)", Optional: []string{"replica", "timeout"}},
	{Name: "sut.kill", Connector: "sut", Doc: "Send a signal (default KILL) — crash without cleanup", Optional: []string{"replica", "signal"}},
	{Name: "sut.start", Connector: "sut", Doc: "Start a stopped instance and wait until healthy", Optional: []string{"replica"}},
	{Name: "sut.pause", Connector: "sut", Doc: "Freeze the process (docker pause)", Optional: []string{"replica"}},
	{Name: "sut.unpause", Connector: "sut", Doc: "Resume a paused process", Optional: []string{"replica"}},
	{Name: "sut.wait_healthy", Connector: "sut", Doc: "Wait until every instance answers its health check", Optional: []string{"replica"}},
}

// Checks documents the check sources.
var Checks = []kit.CheckDef{
	{Prefix: "postgres", Connector: "postgres", Doc: "Rows of entities in the namespace database",
		Examples: []string{"postgres.order.o1.status", "postgres.order.o1.exists", "postgres.order.count(status=paid)"}},
	{Prefix: "kafka", Connector: "kafka", Doc: "Records of namespaced topics and consumer lag",
		Examples: []string{"kafka.order-events.count", "kafka.topic(orders.dlq).count", "kafka.order-events.count(key=o1)", "kafka.lag(order-worker)"}},
	{Prefix: "es", Connector: "elasticsearch", Doc: "Documents (after _refresh)",
		Examples: []string{"es.order.o1.status", "es.order.count(status=paid)"}},
	{Prefix: "mock", Connector: "mock", Doc: "Mock Hub journal of a mock",
		Examples: []string{"mock.payment.calls", "mock.payment.calls(status=201)", "mock.payment.schema_errors", "mock.payment.idempotency_keys"}},
	{Prefix: "mail", Connector: "mail", Doc: "Mails captured by Mailpit",
		Examples: []string{"mail.to(customer).count", "mail.to(customer).subject"}},
	{Prefix: "sut", Connector: "sut", Doc: "Service under test containers",
		Examples: []string{"sut.restarts", "sut.running", "sut.log(order paid).count", "sut.metric(worker_poll_empty_total)"}},
}

// Register adds every adapter, step and check to reg.
func Register(reg *kit.Registry) {
	reg.AddConnector("postgres", postgres.New)
	reg.AddConnector("kafka", kafka.New)
	reg.AddConnector("elasticsearch", elasticsearch.New)
	reg.AddConnector("mock", httpmock.NewConnector)
	reg.AddConnector("mail", mail.New)
	reg.AddConnector("sut", sut.New)
	reg.AddConnector("trigger:kafka", kafka.NewTrigger)
	reg.AddConnector("trigger:db-poll", dbpoll.New)
	for _, d := range Defs {
		reg.AddStep(d)
	}
	for _, c := range Checks {
		reg.AddCheck(c)
	}
}

// Registry returns a registry with everything registered.
func Registry() *kit.Registry {
	r := kit.NewRegistry()
	Register(r)
	return r
}
