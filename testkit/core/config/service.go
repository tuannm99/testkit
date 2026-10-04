package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from "30s"-style strings.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
func (d Duration) D() time.Duration          { return time.Duration(d) }

// Service describes one service under test. Adding a service to TestKit means
// adding one of these files; no core code changes.
type Service struct {
	APIVersion  string                   `yaml:"apiVersion"`
	Kind        string                   `yaml:"kind"`
	Name        string                   `yaml:"name"`
	Description string                   `yaml:"description"`
	Owner       string                   `yaml:"owner"`
	Image       ServiceImage             `yaml:"image"`
	Ports       map[string]int           `yaml:"ports"`
	Health      HealthSpec               `yaml:"health"`
	Metrics     MetricsSpec              `yaml:"metrics"`
	Stores      Stores                   `yaml:"stores"`
	Mocks       map[string]MockSpec      `yaml:"mocks"`
	Triggers    map[string]TriggerSpec   `yaml:"triggers"`
	Entities    map[string]Entity        `yaml:"entities"`
	Env         map[string]string        `yaml:"env"`
	Failpoints  map[string]string        `yaml:"failpoints"` // name -> what breaking it means
	Panels      map[string]PanelSpec     `yaml:"panels"`
	Requires    []string                 `yaml:"requires"` // special privileges (docker.sock, NET_ADMIN)
	Reconcile   map[string]ReconcileSpec `yaml:"reconcile"`

	File string `yaml:"-"` // absolute path of the descriptor
	Dir  string `yaml:"-"`
}

type ServiceImage struct {
	Name    string        `yaml:"name"`
	Tag     string        `yaml:"tag"`
	TestTag string        `yaml:"test_tag"` // image built with the failpoint build tag
	Build   *ServiceBuild `yaml:"build"`
}

type ServiceBuild struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Args       map[string]string `yaml:"args"`
	TestArgs   map[string]string `yaml:"test_args"`
}

type HealthSpec struct {
	Port    string   `yaml:"port"`
	Path    string   `yaml:"path"`
	Timeout Duration `yaml:"timeout"`
}

type MetricsSpec struct {
	Port string `yaml:"port"`
	Path string `yaml:"path"`
}

type Stores struct {
	Postgres      *PostgresStore `yaml:"postgres"`
	Kafka         *KafkaStore    `yaml:"kafka"`
	Elasticsearch *ESStore       `yaml:"elasticsearch"`
	ClickHouse    *CHStore       `yaml:"clickhouse"`
	Mongo         *MongoStore    `yaml:"mongo"`
	Redis         *RedisStore    `yaml:"redis"`
}

// Names returns the declared stores, i.e. the compose services to start.
func (s Stores) Names() []string {
	var out []string
	if s.Postgres != nil {
		out = append(out, "postgres")
	}
	if s.Kafka != nil {
		out = append(out, "kafka")
	}
	if s.Elasticsearch != nil {
		out = append(out, "elasticsearch")
	}
	if s.ClickHouse != nil {
		out = append(out, "clickhouse")
	}
	if s.Mongo != nil {
		out = append(out, "mongo")
	}
	if s.Redis != nil {
		out = append(out, "redis")
	}
	return out
}

type PostgresStore struct {
	Migrations string   `yaml:"migrations"` // directory of *.sql applied in name order
	Snapshot   []string `yaml:"snapshot"`   // tables dumped into evidence after each case
}

type KafkaTopic struct {
	Name       string `yaml:"name"`
	Partitions int    `yaml:"partitions"`
}

type KafkaStore struct {
	Topics []KafkaTopic `yaml:"topics"`
	Groups []string     `yaml:"groups"` // consumer groups of the service (for lag checks)
}

type ESIndex struct {
	Mappings string `yaml:"mappings"` // JSON file with settings/mappings (replicas forced to 0)
}

type ESStore struct {
	Indices map[string]ESIndex `yaml:"indices"`
}

type CHStore struct {
	Migrations string   `yaml:"migrations"`
	Snapshot   []string `yaml:"snapshot"`
}

type MongoStore struct {
	Collections []string `yaml:"collections"`
	Snapshot    []string `yaml:"snapshot"`
}

type RedisStore struct {
	Snapshot bool `yaml:"snapshot"` // dump keys under the run prefix into evidence
}

// MockSpec declares a third party the service talks to.
type MockSpec struct {
	Kind        string `yaml:"kind"`        // http | webhook | smtp | socket
	OpenAPI     string `yaml:"openapi"`     // spec file; requests are validated against it
	Operation   string `yaml:"operation"`   // default operationId for scripted responses
	APIVersion  string `yaml:"api_version"` // version of the real API the mock imitates
	VerifiedAt  string `yaml:"verified_at"` // date the mock was last checked against the real API
	Secret      string `yaml:"secret"`      // test-only signing secret (webhooks)
	Protocol    string `yaml:"protocol"`    // socket: ws | tcp
	Description string `yaml:"description"`
}

// TriggerSpec declares how a job reaches the worker for one trigger kind.
type TriggerSpec struct {
	Topic string `yaml:"topic"` // kafka: logical topic name (namespaced at run time)
	Key   string `yaml:"key"`   // kafka: template of the record key
	Value string `yaml:"value"` // kafka: template of the record value
	SQL   string `yaml:"sql"`   // db-poll: insert statement (template)
	Group string `yaml:"group"` // kafka: consumer group used for lag checks
	Table string `yaml:"table"` // db-poll: job table used for drain checks
	// Drained: SQL returning the number of jobs not yet in a terminal state.
	Drained string `yaml:"drained"`
}

// Entity maps a business object to where it lives in each store, so that a
// check such as postgres.order.o1.status stays declarative.
type Entity struct {
	Postgres      *EntityTable `yaml:"postgres"`
	Elasticsearch *EntityTable `yaml:"elasticsearch"`
	ClickHouse    *EntityTable `yaml:"clickhouse"`
	Mongo         *EntityTable `yaml:"mongo"`
}

type EntityTable struct {
	Table string `yaml:"table"` // table / index / collection (logical name)
	Key   string `yaml:"key"`
}

// ReconcileSpec compares the same set of ids across stores after a run
// (counts, duplicates, missing/extra ids, hash of the sorted id set).
type ReconcileSpec struct {
	Description string            `yaml:"description"`
	Sources     []ReconcileSource `yaml:"sources"`
}

// ReconcileSource extracts ids from one store.
type ReconcileSource struct {
	Store      string         `yaml:"store"`      // postgres | elasticsearch | clickhouse | mongo
	SQL        string         `yaml:"sql"`        // postgres, clickhouse: first column = id
	Index      string         `yaml:"index"`      // elasticsearch (logical name)
	Term       map[string]any `yaml:"term"`       // elasticsearch term filters
	Collection string         `yaml:"collection"` // mongo
	Filter     map[string]any `yaml:"filter"`     // mongo equality filter
	Field      string         `yaml:"field"`      // es / mongo: field holding the id
}

// PanelSpec binds an evidence name to a Grafana panel and the raw PromQL that
// backs it (raw data is the primary evidence, the image is a convenience).
type PanelSpec struct {
	Dashboard string `yaml:"dashboard"` // dashboard uid
	PanelID   int    `yaml:"panel_id"`
	Title     string `yaml:"title"`
	Query     string `yaml:"query"` // PromQL; $run_id / $ns are substituted
	Unit      string `yaml:"unit"`
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)

// LoadService parses and validates one service descriptor.
func LoadService(path string) (*Service, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Service{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.File, _ = filepath.Abs(path)
	s.Dir = filepath.Dir(s.File)
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Validate checks the descriptor without touching any infrastructure.
func (s *Service) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if s.APIVersion != "testkit/v1" || s.Kind != "Service" {
		add("apiVersion must be testkit/v1 and kind Service")
	}
	if !nameRe.MatchString(s.Name) {
		add("name %q must match %s", s.Name, nameRe)
	}
	if s.Image.Name == "" || s.Image.Tag == "" {
		add("image.name and image.tag are required")
	}
	if s.Image.Tag == "latest" || s.Image.TestTag == "latest" {
		add("image tags must be pinned, not latest")
	}
	if s.Health.Path != "" {
		if _, ok := s.Ports[s.Health.Port]; !ok {
			add("health.port %q is not declared in ports", s.Health.Port)
		}
	}
	if s.Metrics.Path != "" {
		if _, ok := s.Ports[s.Metrics.Port]; !ok {
			add("metrics.port %q is not declared in ports", s.Metrics.Port)
		}
	}
	for name, m := range s.Mocks {
		switch m.Kind {
		case "http", "webhook", "smtp", "socket":
		default:
			add("mocks.%s.kind %q must be one of http|webhook|smtp|socket", name, m.Kind)
		}
		if (m.Kind == "http" || m.Kind == "webhook") && (m.APIVersion == "" || m.VerifiedAt == "") {
			add("mocks.%s: api_version and verified_at are required (mock provenance)", name)
		}
		if m.OpenAPI != "" {
			if _, err := os.Stat(s.Path(m.OpenAPI)); err != nil {
				add("mocks.%s.openapi: %v", name, err)
			}
		}
	}
	for name, t := range s.Triggers {
		switch name {
		case "kafka":
			if t.Topic == "" || t.Value == "" {
				add("triggers.kafka needs topic and value")
			}
			if s.Stores.Kafka == nil {
				add("triggers.kafka requires stores.kafka")
			}
		case "db-poll":
			if t.SQL == "" {
				add("triggers.db-poll needs sql")
			}
			if s.Stores.Postgres == nil {
				add("triggers.db-poll requires stores.postgres")
			}
		default:
			add("unknown trigger %q (kafka | db-poll)", name)
		}
	}
	for name, r := range s.Reconcile {
		if len(r.Sources) < 2 {
			add("reconcile.%s needs at least two sources", name)
		}
		for i, src := range r.Sources {
			switch src.Store {
			case "postgres", "clickhouse":
				if src.SQL == "" {
					add("reconcile.%s.sources[%d]: sql is required for %s", name, i, src.Store)
				}
			case "elasticsearch":
				if src.Index == "" || src.Field == "" {
					add("reconcile.%s.sources[%d]: index and field are required", name, i)
				}
			case "mongo":
				if src.Collection == "" || src.Field == "" {
					add("reconcile.%s.sources[%d]: collection and field are required", name, i)
				}
			default:
				add("reconcile.%s.sources[%d]: unknown store %q", name, i, src.Store)
			}
		}
	}
	if pg := s.Stores.Postgres; pg != nil && pg.Migrations != "" {
		if _, err := os.Stat(s.Path(pg.Migrations)); err != nil {
			add("stores.postgres.migrations: %v", err)
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid service descriptor:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Path resolves a path relative to the descriptor file.
func (s *Service) Path(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(s.Dir, rel)
}

// ImageRef returns the image to run: the failpoint-enabled test build when
// test is true and one is declared.
func (s *Service) ImageRef(test bool) string {
	if test && s.Image.TestTag != "" {
		return s.Image.Name + ":" + s.Image.TestTag
	}
	return s.Image.Name + ":" + s.Image.Tag
}

// MockKinds lists the infra components needed by the declared mocks.
func (s *Service) MockComponents() []string {
	set := map[string]bool{}
	for _, m := range s.Mocks {
		set["mockhub"] = true
		if m.Kind == "smtp" {
			set["mailpit"] = true
		}
	}
	return SortedKeys(set)
}

// LoadServices loads every *.yaml in dir, keyed by service name.
func LoadServices(dir string) (map[string]*Service, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	out := map[string]*Service{}
	for _, f := range files {
		s, err := LoadService(f)
		if err != nil {
			return nil, err
		}
		if prev, dup := out[s.Name]; dup {
			return nil, fmt.Errorf("service %q declared twice (%s, %s)", s.Name, prev.File, f)
		}
		out[s.Name] = s
	}
	return out, nil
}
