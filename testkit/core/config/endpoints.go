package config

import (
	"fmt"
	"os"
	"strings"
)

// PG holds the Postgres connection parameters (server level, no database).
type PG struct {
	Host     string
	Port     string
	User     string
	Password string
}

// DSN builds a URL DSN for one database.
func (p PG) DSN(db string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", p.User, p.Password, p.Host, p.Port, db)
}

// Endpoints is the address book of the infrastructure. The same infra is seen
// from two places: from inside the docker network (services under test, the
// containerised runner) and from the host (a runner started with `go run`).
type Endpoints struct {
	InNetwork bool

	Postgres      PG
	KafkaBrokers  []string
	Elasticsearch string
	ClickHouse    string
	CHUser        string
	CHPassword    string
	Mongo         string
	Redis         string
	RabbitAMQP    string // host:port of the AMQP listener
	RabbitMgmt    string // management HTTP API base URL
	RabbitUser    string
	RabbitPass    string
	Mockhub       string
	MockhubSMTP   string
	MockhubSocket string
	Mailpit       string
	MailpitSMTP   string
	Toxiproxy     string
	Prometheus    string
	Grafana       string
	GrafanaUser   string
	GrafanaPass   string
	Loki          string
	Tempo         string
	OTLP          string
}

// RunnerInNetwork reports whether this process runs inside the TestKit docker
// network (the `tk` wrapper sets TESTKIT_IN_NETWORK=1).
func RunnerInNetwork() bool {
	if v := os.Getenv("TESTKIT_IN_NETWORK"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return false
}

// Endpoints returns addresses as seen from inside the network (inNetwork) or
// from the host through the 127.0.0.1-published ports.
func (p *Project) Endpoints(inNetwork bool) Endpoints {
	hp := func(service, containerPort, portKey string) string {
		if inNetwork {
			return service + ":" + containerPort
		}
		return "127.0.0.1:" + p.Get(portKey)
	}
	e := Endpoints{
		InNetwork:     inNetwork,
		KafkaBrokers:  []string{hp("kafka", "9092", "TK_PORT_KAFKA")},
		Elasticsearch: "http://" + hp("elasticsearch", "9200", "TK_PORT_ELASTICSEARCH"),
		ClickHouse:    "http://" + hp("clickhouse", "8123", "TK_PORT_CLICKHOUSE"),
		CHUser:        p.Get("TK_CLICKHOUSE_USER"),
		CHPassword:    p.Get("TK_CLICKHOUSE_PASSWORD"),
		Redis:         hp("redis", "6379", "TK_PORT_REDIS"),
		RabbitAMQP:    hp("rabbitmq", "5672", "TK_PORT_RABBITMQ"),
		RabbitMgmt:    "http://" + hp("rabbitmq", "15672", "TK_PORT_RABBITMQ_MGMT"),
		RabbitUser:    p.Get("TK_RABBITMQ_USER"),
		RabbitPass:    p.Get("TK_RABBITMQ_PASSWORD"),
		Mockhub:       "http://" + hp("mockhub", "8081", "TK_PORT_MOCKHUB"),
		MockhubSMTP:   hp("mockhub", "2525", "TK_PORT_MOCKHUB_SMTP"),
		MockhubSocket: hp("mockhub", "8082", "TK_PORT_MOCKHUB_SOCKET"),
		Mailpit:       "http://" + hp("mailpit", "8025", "TK_PORT_MAILPIT"),
		MailpitSMTP:   hp("mailpit", "1025", "TK_PORT_MAILPIT_SMTP"),
		Toxiproxy:     "http://" + hp("toxiproxy", "8474", "TK_PORT_TOXIPROXY"),
		Prometheus:    "http://" + hp("prometheus", "9090", "TK_PORT_PROMETHEUS"),
		Grafana:       "http://" + hp("grafana", "3000", "TK_PORT_GRAFANA"),
		GrafanaUser:   p.Get("TK_GRAFANA_ADMIN_USER"),
		GrafanaPass:   p.Get("TK_GRAFANA_ADMIN_PASSWORD"),
		Loki:          "http://" + hp("loki", "3100", "TK_PORT_LOKI"),
		Tempo:         "http://" + hp("tempo", "3200", "TK_PORT_TEMPO"),
		OTLP:          "otel-collector:4317",
	}
	pgHostPort := hp("postgres", "5432", "TK_PORT_POSTGRES")
	host, port, _ := strings.Cut(pgHostPort, ":")
	e.Postgres = PG{Host: host, Port: port, User: p.Get("TK_POSTGRES_USER"), Password: p.Get("TK_POSTGRES_PASSWORD")}
	mongo := hp("mongo", "27017", "TK_PORT_MONGO")
	if inNetwork {
		e.Mongo = "mongodb://" + mongo + "/?replicaSet=rs0"
	} else {
		// The replica-set member is advertised as mongo:27017, unreachable from the host.
		e.Mongo = "mongodb://" + mongo + "/?directConnection=true"
	}
	return e
}
